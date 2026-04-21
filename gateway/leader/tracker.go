package leader

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// NodeStatus represents the status reported by a replica's /status endpoint.
type NodeStatus struct {
	ReplicaID       string `json:"replicaId"`
	State           string `json:"state"`
	Term            int64  `json:"term"`
	LogLength       int    `json:"logLength"`
	CommitIndex     int64  `json:"commitIndex"`
	LeaderID        string `json:"leaderId"`
	LastHeartbeatMs int64  `json:"lastHeartbeatMs"`
	Healthy         bool   `json:"healthy"`
}

// ReplicaConfig holds the connection details for a single replica.
type ReplicaConfig struct {
	ID         string
	StatusURL  string // e.g. http://replica1:8081/status
	StrokeURL  string // e.g. http://replica1:8081/stroke
	EntriesURL string // e.g. http://replica1:8081/entries
}

// LeaderTracker polls all replica /status endpoints and tracks the current leader.
type LeaderTracker struct {
	mu       sync.RWMutex
	leaderID string
	term     int64
	replicas []ReplicaConfig
	statuses map[string]NodeStatus // replicaID -> last known status
	logger   *zap.Logger
	onChange func(newLeaderID string, term int64) // called on leader change
}

// NewLeaderTracker creates a new LeaderTracker.
func NewLeaderTracker(replicas []ReplicaConfig, logger *zap.Logger, onChange func(string, int64)) *LeaderTracker {
	return &LeaderTracker{
		replicas: replicas,
		statuses: make(map[string]NodeStatus),
		logger:   logger,
		onChange: onChange,
	}
}

// Start launches a background goroutine that polls all replicas every 500ms.
func (t *LeaderTracker) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.poll()
			}
		}
	}()
}

// poll fetches status from all replicas concurrently and updates the tracked leader.
func (t *LeaderTracker) poll() {
	type result struct {
		replicaID string
		status    NodeStatus
		err       error
	}

	httpClient := &http.Client{Timeout: 300 * time.Millisecond}
	resultCh := make(chan result, len(t.replicas))

	// Fan out: one goroutine per replica (not sequential).
	for _, replica := range t.replicas {
		go func(r ReplicaConfig) {
			s, err := fetchStatus(httpClient, r)
			resultCh <- result{replicaID: r.ID, status: s, err: err}
		}(replica)
	}

	var (
		bestLeaderID string
		bestTerm     int64 = -1
	)

	// Collect all results.
	for i := 0; i < len(t.replicas); i++ {
		res := <-resultCh

		t.mu.Lock()
		if res.err != nil {
			t.logger.Debug("failed to fetch replica status",
				zap.String("replica", res.replicaID),
				zap.Error(res.err),
			)
			if prev, ok := t.statuses[res.replicaID]; ok {
				prev.Healthy = false
				t.statuses[res.replicaID] = prev
			} else {
				t.statuses[res.replicaID] = NodeStatus{
					ReplicaID: res.replicaID,
					Healthy:   false,
				}
			}
		} else {
			t.statuses[res.replicaID] = res.status
			if res.status.State == "LEADER" && res.status.Term > bestTerm {
				bestTerm = res.status.Term
				bestLeaderID = res.status.ReplicaID
			}
		}
		t.mu.Unlock()
	}

	// If no replica reports LEADER directly, fall back to leaderId field.
	if bestLeaderID == "" {
		t.mu.RLock()
		for _, s := range t.statuses {
			if s.Healthy && s.LeaderID != "" && s.Term > bestTerm {
				bestTerm = s.Term
				bestLeaderID = s.LeaderID
			}
		}
		t.mu.RUnlock()
	}

	t.mu.Lock()
	prevLeader := t.leaderID
	prevTerm := t.term
	if bestLeaderID != "" {
		t.leaderID = bestLeaderID
		t.term = bestTerm
	}
	newLeader := t.leaderID
	newTerm := t.term
	t.mu.Unlock()

	if (newLeader != prevLeader || newTerm != prevTerm) && newLeader != "" && t.onChange != nil {
		t.logger.Info("leader changed",
			zap.String("from", prevLeader),
			zap.String("to", newLeader),
			zap.Int64("term", newTerm),
		)
		t.onChange(newLeader, newTerm)
	}
}

// fetchStatus performs an HTTP GET to a replica's status endpoint.
func fetchStatus(client *http.Client, replica ReplicaConfig) (NodeStatus, error) {
	resp, err := client.Get(replica.StatusURL)
	if err != nil {
		return NodeStatus{}, err
	}
	defer resp.Body.Close()

	var status NodeStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return NodeStatus{}, err
	}
	status.Healthy = true
	return status, nil
}

// GetLeaderID returns the currently tracked leader ID.
func (t *LeaderTracker) GetLeaderID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.leaderID
}

// GetLeaderConfig returns the ReplicaConfig for the current leader.
func (t *LeaderTracker) GetLeaderConfig() (ReplicaConfig, bool) {
	t.mu.RLock()
	leaderID := t.leaderID
	t.mu.RUnlock()
	return t.GetReplicaConfig(leaderID)
}

// GetTerm returns the current known term.
func (t *LeaderTracker) GetTerm() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.term
}

// GetAllStatuses returns a snapshot of all known replica statuses.
func (t *LeaderTracker) GetAllStatuses() []NodeStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]NodeStatus, 0, len(t.statuses))
	for _, s := range t.statuses {
		result = append(result, s)
	}
	return result
}

// GetReplicaConfig finds a ReplicaConfig by ID.
func (t *LeaderTracker) GetReplicaConfig(id string) (ReplicaConfig, bool) {
	for _, r := range t.replicas {
		if r.ID == id {
			return r, true
		}
	}
	return ReplicaConfig{}, false
}
