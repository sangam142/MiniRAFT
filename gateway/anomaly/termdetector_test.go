package anomaly

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

type testMetrics struct {
	termSurges int
	spread     float64
}

func (m *testMetrics) IncrTermSurges()         { m.termSurges++ }
func (m *testMetrics) SetTermSpread(v float64) { m.spread = v }

func TestTermDetectorDetectsAndResolves(t *testing.T) {
	events := make([]string, 0)
	m := &testMetrics{}
	d := NewTermDetector(zap.NewNop(), m, func(event string, payload interface{}) {
		events = append(events, event)
	})

	t0 := time.UnixMilli(1000)
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 2, State: "FOLLOWER"},
		{ReplicaID: "replica2", Healthy: true, Term: 2, State: "LEADER"},
		{ReplicaID: "replica3", Healthy: true, Term: 2, State: "FOLLOWER"},
	}, t0)

	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 6, State: "CANDIDATE"},
		{ReplicaID: "replica2", Healthy: true, Term: 2, State: "LEADER"},
		{ReplicaID: "replica3", Healthy: true, Term: 2, State: "FOLLOWER"},
	}, t0.Add(900*time.Millisecond))

	if len(events) != 1 || events[0] != "term_surge_detected" {
		t.Fatalf("expected one detect event, got=%v", events)
	}
	if m.termSurges != 1 {
		t.Fatalf("expected term surge metric increment, got=%d", m.termSurges)
	}

	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 6, State: "FOLLOWER"},
		{ReplicaID: "replica2", Healthy: true, Term: 6, State: "LEADER"},
		{ReplicaID: "replica3", Healthy: true, Term: 6, State: "FOLLOWER"},
	}, t0.Add(4*time.Second))

	if len(events) != 2 || events[1] != "term_surge_resolved" {
		t.Fatalf("expected resolve event after stabilization, got=%v", events)
	}
}

func TestTermDetectorSpreadOnly(t *testing.T) {
	events := make([]string, 0)
	m := &testMetrics{}
	d := NewTermDetector(zap.NewNop(), m, func(event string, payload interface{}) {
		events = append(events, event)
	})

	t0 := time.UnixMilli(2000)
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 5, State: "LEADER"},
		{ReplicaID: "replica2", Healthy: true, Term: 5, State: "FOLLOWER"},
		{ReplicaID: "replica3", Healthy: true, Term: 5, State: "FOLLOWER"},
	}, t0)

	// Spread-only trigger: maxDelta remains below threshold while term spread is 2.
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 5, State: "LEADER"},
		{ReplicaID: "replica2", Healthy: true, Term: 5, State: "FOLLOWER"},
		{ReplicaID: "replica3", Healthy: true, Term: 3, State: "FOLLOWER"},
	}, t0.Add(500*time.Millisecond))

	if len(events) != 1 || events[0] != "term_surge_detected" {
		t.Fatalf("expected spread-only detect event, got=%v", events)
	}
	if m.spread != 2 {
		t.Fatalf("expected term spread metric 2, got=%v", m.spread)
	}
}

func TestTermDetectorThresholdBoundary(t *testing.T) {
	events := make([]string, 0)
	m := &testMetrics{}
	d := NewTermDetector(zap.NewNop(), m, func(event string, payload interface{}) {
		events = append(events, event)
	})

	t0 := time.UnixMilli(3000)
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 5, State: "LEADER"},
		{ReplicaID: "replica2", Healthy: true, Term: 5, State: "FOLLOWER"},
		{ReplicaID: "replica3", Healthy: true, Term: 5, State: "FOLLOWER"},
	}, t0)

	// Delta=2 boundary should not trigger (threshold is 3).
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 7, State: "LEADER"},
		{ReplicaID: "replica2", Healthy: true, Term: 7, State: "FOLLOWER"},
		{ReplicaID: "replica3", Healthy: true, Term: 7, State: "FOLLOWER"},
	}, t0.Add(800*time.Millisecond))

	if len(events) != 0 {
		t.Fatalf("expected no surge event at delta=2, got=%v", events)
	}

	// Delta=3 boundary should trigger.
	d.Observe([]leader.NodeStatus{
		{ReplicaID: "replica1", Healthy: true, Term: 8, State: "LEADER"},
		{ReplicaID: "replica2", Healthy: true, Term: 8, State: "FOLLOWER"},
		{ReplicaID: "replica3", Healthy: true, Term: 8, State: "FOLLOWER"},
	}, t0.Add(1200*time.Millisecond))

	if len(events) != 1 || events[0] != "term_surge_detected" {
		t.Fatalf("expected surge event at delta=3, got=%v", events)
	}
}
