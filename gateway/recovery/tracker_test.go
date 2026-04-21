package recovery

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

type trackerMetrics struct {
	runs        int
	active      bool
	recoveryObs int
	electionObs int
}

func (m *trackerMetrics) IncrRecoveryRuns()                  { m.runs++ }
func (m *trackerMetrics) SetRecoveryActive(active bool)      { m.active = active }
func (m *trackerMetrics) ObserveRecoveryDuration(ms float64) { m.recoveryObs++ }
func (m *trackerMetrics) ObserveElectionDuration(ms float64) { m.electionObs++ }

func TestTrackerRunLifecycle(t *testing.T) {
	var stats BufferStats
	events := make([]string, 0)
	m := &trackerMetrics{}

	tr := NewTracker(zap.NewNop(), m, func(event string, payload interface{}) {
		events = append(events, event)
	}, func() BufferStats {
		return stats
	})

	start := time.UnixMilli(1000)
	runID := tr.StartRun("replica1", "hard", "miniraft-replica1-1", start)
	if runID == "" {
		t.Fatalf("expected runID")
	}
	if m.runs != 1 || !m.active {
		t.Fatalf("expected active recovery run metrics updated")
	}

	tr.ObserveLeader("replica2", 8, start.Add(350*time.Millisecond))
	tr.RecordCommitLatency(start.Add(200*time.Millisecond).UnixMilli(), start.Add(600*time.Millisecond))

	stats = BufferStats{BufferedTotal: 5, DroppedTotal: 1}
	statuses := []leader.NodeStatus{
		{ReplicaID: "replica2", State: "LEADER", LeaderID: "replica2", Healthy: true},
		{ReplicaID: "replica1", State: "FOLLOWER", LeaderID: "replica2", Healthy: true},
		{ReplicaID: "replica3", State: "FOLLOWER", LeaderID: "replica2", Healthy: true},
	}
	tr.ObserveStatuses(statuses, start.Add(900*time.Millisecond))
	tr.ObserveStatuses(statuses, start.Add(1300*time.Millisecond))

	if tr.ActiveRunID() != "" {
		t.Fatalf("expected no active run after stability")
	}
	if m.active {
		t.Fatalf("expected active gauge reset")
	}
	if m.recoveryObs == 0 || m.electionObs == 0 {
		t.Fatalf("expected metrics observations for election and recovery")
	}
	if len(events) < 3 {
		t.Fatalf("expected started/milestone/completed events, got=%v", events)
	}
	if events[0] != "chaos_recovery_started" {
		t.Fatalf("first event should be start, got=%s", events[0])
	}
	if events[len(events)-1] != "chaos_recovery_completed" {
		t.Fatalf("last event should be completed, got=%s", events[len(events)-1])
	}
}

func TestTrackerCompletesWithoutLeaderChange(t *testing.T) {
	events := make([]string, 0)
	m := &trackerMetrics{}

	tr := NewTracker(zap.NewNop(), m, func(event string, payload interface{}) {
		events = append(events, event)
	}, func() BufferStats {
		return BufferStats{}
	})

	start := time.UnixMilli(2000)
	runID := tr.StartRun("replica1", "graceful", "miniraft-replica1-1", start)
	if runID == "" {
		t.Fatalf("expected runID")
	}

	statuses := []leader.NodeStatus{
		{ReplicaID: "replica2", State: "LEADER", LeaderID: "replica2", Healthy: true, Term: 7},
		{ReplicaID: "replica1", State: "FOLLOWER", LeaderID: "replica2", Healthy: true, Term: 7},
		{ReplicaID: "replica3", State: "FOLLOWER", LeaderID: "replica2", Healthy: true, Term: 7},
	}

	tr.ObserveStatuses(statuses, start.Add(300*time.Millisecond))
	tr.ObserveStatuses(statuses, start.Add(700*time.Millisecond))

	if tr.ActiveRunID() != "" {
		t.Fatalf("expected run to complete without explicit leader change callback")
	}
	if m.recoveryObs == 0 || m.electionObs == 0 {
		t.Fatalf("expected election/recovery observations when run completes")
	}
	if len(events) < 2 || events[len(events)-1] != "chaos_recovery_completed" {
		t.Fatalf("expected completed event, got=%v", events)
	}
}
