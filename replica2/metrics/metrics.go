package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ReplicaMetrics holds all Prometheus metrics for the replica.
type ReplicaMetrics struct {
	RaftTerm              prometheus.Gauge
	RaftState             prometheus.Gauge
	RaftElectionsTotal    prometheus.Counter
	RaftVotesGranted      prometheus.Counter
	RaftHeartbeatsSent    prometheus.Counter
	RaftHeartbeatLatency  prometheus.Histogram
	RaftLogLength         prometheus.Gauge
	RaftCommitIndex       prometheus.Gauge
	RaftEntriesAppended   prometheus.Counter
	RaftEntriesCommitted  prometheus.Counter
	RaftWALWritesTotal    prometheus.Counter
}

// NewReplicaMetrics creates and registers all Prometheus metrics for the given replicaID.
func NewReplicaMetrics(replicaID string) *ReplicaMetrics {
	labels := prometheus.Labels{"replica_id": replicaID}

	m := &ReplicaMetrics{
		RaftTerm: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "raft_term_total",
			Help:        "Current RAFT term.",
			ConstLabels: labels,
		}),
		RaftState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "raft_state",
			Help:        "Current RAFT node state: 0=follower, 1=candidate, 2=leader.",
			ConstLabels: labels,
		}),
		RaftElectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_elections_total",
			Help:        "Total number of elections started.",
			ConstLabels: labels,
		}),
		RaftVotesGranted: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_votes_granted_total",
			Help:        "Total number of votes granted.",
			ConstLabels: labels,
		}),
		RaftHeartbeatsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_heartbeats_sent_total",
			Help:        "Total number of heartbeats sent.",
			ConstLabels: labels,
		}),
		RaftHeartbeatLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "raft_heartbeat_latency_seconds",
			Help:        "Heartbeat round-trip latency in seconds.",
			ConstLabels: labels,
			Buckets:     prometheus.DefBuckets,
		}),
		RaftLogLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "raft_log_length",
			Help:        "Current number of entries in the RAFT log.",
			ConstLabels: labels,
		}),
		RaftCommitIndex: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "raft_commit_index",
			Help:        "Current commit index.",
			ConstLabels: labels,
		}),
		RaftEntriesAppended: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_entries_appended_total",
			Help:        "Total number of log entries appended.",
			ConstLabels: labels,
		}),
		RaftEntriesCommitted: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_entries_committed_total",
			Help:        "Total number of log entries committed.",
			ConstLabels: labels,
		}),
		RaftWALWritesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "raft_wal_writes_total",
			Help:        "Total number of WAL writes.",
			ConstLabels: labels,
		}),
	}

	prometheus.MustRegister(
		m.RaftTerm,
		m.RaftState,
		m.RaftElectionsTotal,
		m.RaftVotesGranted,
		m.RaftHeartbeatsSent,
		m.RaftHeartbeatLatency,
		m.RaftLogLength,
		m.RaftCommitIndex,
		m.RaftEntriesAppended,
		m.RaftEntriesCommitted,
		m.RaftWALWritesTotal,
	)

	return m
}

// IncrWALWrites increments WAL write counter.
func (m *ReplicaMetrics) IncrWALWrites() {
	m.RaftWALWritesTotal.Inc()
}
