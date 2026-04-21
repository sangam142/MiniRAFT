package anomaly

import (
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

type termSample struct {
	At   time.Time
	Term int64
}

// TermDetector watches status samples and emits term surge events.
type TermDetector struct {
	logger *zap.Logger
	history map[string][]termSample

	active      bool
	activeSince time.Time
	lastReason  string

	window          time.Duration
	thresholdDelta  int64
	thresholdSpread int64

	emit func(event string, payload interface{})
	metrics interface {
		IncrTermSurges()
		SetTermSpread(float64)
	}
}

// NewTermDetector creates a detector with conservative defaults.
func NewTermDetector(logger *zap.Logger, metrics interface {
	IncrTermSurges()
	SetTermSpread(float64)
}, emit func(event string, payload interface{})) *TermDetector {
	return &TermDetector{
		logger:          logger,
		history:         make(map[string][]termSample),
		window:          2 * time.Second,
		thresholdDelta:  3,
		thresholdSpread: 2,
		emit:            emit,
		metrics:         metrics,
	}
}

// Observe ingests a full status snapshot and emits detected/resolved events.
func (d *TermDetector) Observe(statuses []leader.NodeStatus, now time.Time) {
	if len(statuses) == 0 {
		return
	}

	terms := make(map[string]int64, len(statuses))
	states := make(map[string]string, len(statuses))
	healthyTerms := make([]int64, 0, len(statuses))

	var (
		maxDelta       int64
		maxDeltaReplica string
	)

	for _, s := range statuses {
		terms[s.ReplicaID] = s.Term
		states[s.ReplicaID] = s.State
		if s.Healthy {
			healthyTerms = append(healthyTerms, s.Term)
		}

		h := append(d.history[s.ReplicaID], termSample{At: now, Term: s.Term})
		cutoff := now.Add(-d.window)
		keep := h[:0]
		for _, sample := range h {
			if !sample.At.Before(cutoff) {
				keep = append(keep, sample)
			}
		}
		h = keep
		d.history[s.ReplicaID] = h

		if len(h) >= 2 {
			delta := h[len(h)-1].Term - h[0].Term
			if delta > maxDelta {
				maxDelta = delta
				maxDeltaReplica = s.ReplicaID
			}
		}
	}

	spread := int64(0)
	if len(healthyTerms) > 0 {
		minTerm := healthyTerms[0]
		maxTerm := healthyTerms[0]
		for _, term := range healthyTerms[1:] {
			if term < minTerm {
				minTerm = term
			}
			if term > maxTerm {
				maxTerm = term
			}
		}
		spread = maxTerm - minTerm
	}

	if d.metrics != nil {
		d.metrics.SetTermSpread(float64(spread))
	}

	surge := maxDelta >= d.thresholdDelta || spread >= d.thresholdSpread
	reason := ""
	if maxDelta >= d.thresholdDelta {
		reason = "term_jump"
	} else if spread >= d.thresholdSpread {
		reason = "term_spread"
	}

	if surge && !d.active {
		d.active = true
		d.activeSince = now
		d.lastReason = reason
		if d.metrics != nil {
			d.metrics.IncrTermSurges()
		}
		if d.emit != nil {
			d.emit("term_surge_detected", map[string]interface{}{
				"at":               now.UnixMilli(),
				"reason":           reason,
				"maxDelta":         maxDelta,
				"maxDeltaReplica":  maxDeltaReplica,
				"termSpread":       spread,
				"replicaTerms":     terms,
				"replicaStates":    states,
			})
		}
		d.logger.Warn("term surge detected",
			zap.String("reason", reason),
			zap.Int64("maxDelta", maxDelta),
			zap.Int64("spread", spread),
		)
		return
	}

	if !surge && d.active {
		durationMs := now.Sub(d.activeSince).Milliseconds()
		if d.emit != nil {
			d.emit("term_surge_resolved", map[string]interface{}{
				"at":             now.UnixMilli(),
				"durationMs":     durationMs,
				"previousReason": d.lastReason,
				"termSpread":     spread,
				"replicaTerms":   terms,
				"replicaStates":  states,
			})
		}
		d.logger.Info("term surge resolved",
			zap.Int64("durationMs", durationMs),
			zap.String("reason", d.lastReason),
		)
		d.active = false
		d.activeSince = time.Time{}
		d.lastReason = ""
	}
}
