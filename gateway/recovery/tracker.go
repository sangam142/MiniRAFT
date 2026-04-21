package recovery

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

// BufferStats represents gateway buffering counters.
type BufferStats struct {
	BufferedTotal uint64 `json:"bufferedTotal"`
	DroppedTotal  uint64 `json:"droppedTotal"`
}

// Milestone is emitted during a recovery run.
type Milestone struct {
	Name      string `json:"name"`
	Timestamp int64  `json:"timestamp"`
	Detail    string `json:"detail,omitempty"`
}

// RunSummary is the exported state used by SSE and dashboard UI.
type RunSummary struct {
	RunID              string      `json:"runId"`
	Target             string      `json:"target"`
	Mode               string      `json:"mode"`
	Container          string      `json:"container"`
	Status             string      `json:"status"`
	StartedAt          int64       `json:"startedAt"`
	CompletedAt        int64       `json:"completedAt,omitempty"`
	LeaderID           string      `json:"leaderId,omitempty"`
	Term               int64       `json:"term,omitempty"`
	ElectionMs         int64       `json:"electionMs,omitempty"`
	RecoveryMs         int64       `json:"recoveryMs,omitempty"`
	BufferedDuringRun  int64       `json:"bufferedDuringRun,omitempty"`
	DroppedDuringRun   int64       `json:"droppedDuringRun,omitempty"`
	CommitLatencyP95Ms int64       `json:"commitLatencyP95Ms,omitempty"`
	Milestones         []Milestone `json:"milestones"`
}

type metricsIface interface {
	IncrRecoveryRuns()
	SetRecoveryActive(active bool)
	ObserveRecoveryDuration(ms float64)
	ObserveElectionDuration(ms float64)
}

type runState struct {
	summary         RunSummary
	start           time.Time
	stableTicks     int
	healthyObserved bool
	leaderObserved  bool
	startBuffer     BufferStats
	commitLatencies []int64
}

// Tracker records chaos recovery progression and emits SSE payloads.
type Tracker struct {
	mu       sync.Mutex
	logger   *zap.Logger
	metrics  metricsIface
	emit     func(event string, payload interface{})
	snapshot func() BufferStats

	seq    uint64
	active *runState
	last   *RunSummary
}

// NewTracker creates a recovery tracker.
func NewTracker(logger *zap.Logger, metrics metricsIface, emit func(event string, payload interface{}), snapshot func() BufferStats) *Tracker {
	if snapshot == nil {
		snapshot = func() BufferStats { return BufferStats{} }
	}
	return &Tracker{
		logger:   logger,
		metrics:  metrics,
		emit:     emit,
		snapshot: snapshot,
	}
}

// StartRun starts a new recovery run and emits chaos_recovery_started.
func (t *Tracker) StartRun(target, mode, container string, at time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.seq++
	runID := fmt.Sprintf("run-%d", at.UnixMilli())
	if t.seq > 1 {
		runID = fmt.Sprintf("%s-%d", runID, t.seq)
	}

	r := &runState{
		summary: RunSummary{
			RunID:      runID,
			Target:     target,
			Mode:       mode,
			Container:  container,
			Status:     "running",
			StartedAt:  at.UnixMilli(),
			Milestones: []Milestone{},
		},
		start:           at,
		startBuffer:     t.snapshot(),
		commitLatencies: make([]int64, 0, 32),
	}
	t.active = r
	t.addMilestoneLocked(r, "chaos_started", "chaos action executed", at)

	if t.metrics != nil {
		t.metrics.IncrRecoveryRuns()
		t.metrics.SetRecoveryActive(true)
	}
	if t.emit != nil {
		t.emit("chaos_recovery_started", r.summary)
	}
	return runID
}

// ObserveLeader records first leader election during an active run.
func (t *Tracker) ObserveLeader(leaderID string, term int64, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return
	}
	r := t.active
	r.summary.LeaderID = leaderID
	r.summary.Term = term

	if !r.leaderObserved {
		r.leaderObserved = true
		r.summary.ElectionMs = at.Sub(r.start).Milliseconds()
		t.addMilestoneLocked(r, "leader_elected", "leader="+leaderID, at)
		if t.metrics != nil {
			t.metrics.ObserveElectionDuration(float64(r.summary.ElectionMs))
		}
		if t.emit != nil {
			t.emit("chaos_recovery_milestone", map[string]interface{}{
				"runId":      r.summary.RunID,
				"milestone":  "leader_elected",
				"at":         at.UnixMilli(),
				"leaderId":   leaderID,
				"term":       term,
				"electionMs": r.summary.ElectionMs,
			})
		}
	}
}

// ObserveStatuses advances run stability and emits completion when stabilized.
func (t *Tracker) ObserveStatuses(statuses []leader.NodeStatus, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil || len(statuses) == 0 {
		return
	}
	r := t.active

	allHealthy := true
	leaderID := r.summary.LeaderID
	leaderTerm := r.summary.Term
	leaderCount := 0

	for _, s := range statuses {
		if !s.Healthy {
			allHealthy = false
		}
		if s.State == "LEADER" {
			leaderCount++
			if leaderID == "" {
				leaderID = s.ReplicaID
			}
			if s.Term > 0 {
				leaderTerm = s.Term
			}
		}
	}
	if leaderID != "" {
		r.summary.LeaderID = leaderID
	}
	if leaderTerm > 0 {
		r.summary.Term = leaderTerm
	}

	if !r.leaderObserved && leaderID != "" {
		r.leaderObserved = true
		r.summary.ElectionMs = at.Sub(r.start).Milliseconds()
		t.addMilestoneLocked(r, "leader_observed", "leader="+leaderID, at)
		if t.metrics != nil {
			t.metrics.ObserveElectionDuration(float64(r.summary.ElectionMs))
		}
		if t.emit != nil {
			t.emit("chaos_recovery_milestone", map[string]interface{}{
				"runId":      r.summary.RunID,
				"milestone":  "leader_observed",
				"at":         at.UnixMilli(),
				"leaderId":   leaderID,
				"term":       r.summary.Term,
				"electionMs": r.summary.ElectionMs,
			})
		}
	}

	aligned := allHealthy && leaderID != ""
	if aligned {
		for _, s := range statuses {
			if !s.Healthy {
				continue
			}
			if s.State == "LEADER" && s.ReplicaID != leaderID {
				aligned = false
				break
			}
			if s.State != "LEADER" && s.LeaderID != "" && s.LeaderID != leaderID {
				aligned = false
				break
			}
		}
	}
	if leaderCount != 1 {
		aligned = false
	}

	if allHealthy && !r.healthyObserved {
		r.healthyObserved = true
		t.addMilestoneLocked(r, "followers_healthy", "all replicas healthy", at)
		if t.emit != nil {
			t.emit("chaos_recovery_milestone", map[string]interface{}{
				"runId":     r.summary.RunID,
				"milestone": "followers_healthy",
				"at":        at.UnixMilli(),
			})
		}
	}

	if r.leaderObserved && aligned {
		r.stableTicks++
	} else {
		r.stableTicks = 0
	}

	if r.stableTicks >= 2 {
		t.finishLocked(r, at)
	}
}

// RecordCommitLatency records commit latency while a run is active.
func (t *Tracker) RecordCommitLatency(entryTimestampMs int64, now time.Time) {
	if entryTimestampMs <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return
	}
	latency := now.UnixMilli() - entryTimestampMs
	if latency < 0 {
		latency = 0
	}
	t.active.commitLatencies = append(t.active.commitLatencies, latency)
}

// ActiveRunID returns active run ID if any.
func (t *Tracker) ActiveRunID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		return ""
	}
	return t.active.summary.RunID
}

func (t *Tracker) addMilestoneLocked(r *runState, name, detail string, at time.Time) {
	r.summary.Milestones = append(r.summary.Milestones, Milestone{
		Name:      name,
		Timestamp: at.UnixMilli(),
		Detail:    detail,
	})
}

func (t *Tracker) finishLocked(r *runState, at time.Time) {
	t.addMilestoneLocked(r, "replication_stable", "leader/follower alignment stable", at)
	t.addMilestoneLocked(r, "recovery_completed", "recovery run complete", at)

	r.summary.Status = "completed"
	r.summary.CompletedAt = at.UnixMilli()
	r.summary.RecoveryMs = at.Sub(r.start).Milliseconds()

	endBuffer := t.snapshot()
	r.summary.BufferedDuringRun = int64(endBuffer.BufferedTotal - r.startBuffer.BufferedTotal)
	r.summary.DroppedDuringRun = int64(endBuffer.DroppedTotal - r.startBuffer.DroppedTotal)
	r.summary.CommitLatencyP95Ms = p95(r.commitLatencies)

	summaryCopy := r.summary
	t.last = &summaryCopy
	t.active = nil

	if t.metrics != nil {
		t.metrics.SetRecoveryActive(false)
		t.metrics.ObserveRecoveryDuration(float64(summaryCopy.RecoveryMs))
	}
	if t.emit != nil {
		t.emit("chaos_recovery_completed", summaryCopy)
	}
	t.logger.Info("chaos recovery run completed",
		zap.String("runId", summaryCopy.RunID),
		zap.Int64("recoveryMs", summaryCopy.RecoveryMs),
		zap.Int64("electionMs", summaryCopy.ElectionMs),
	)
}

func p95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	copyVals := append([]int64(nil), values...)
	sort.Slice(copyVals, func(i, j int) bool { return copyVals[i] < copyVals[j] })
	idx := int(float64(len(copyVals)-1) * 0.95)
	return copyVals[idx]
}
