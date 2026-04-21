package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// GatewayMetrics holds all Prometheus metrics for the gateway service.
type GatewayMetrics struct {
	WebsocketConnections prometheus.Gauge
	StrokesForwarded     prometheus.Counter
	StrokesBuffered      prometheus.Counter
	StrokesDropped       prometheus.Counter
	LeaderChanges        prometheus.Counter
	ChaosEvents          *prometheus.CounterVec
	ChaosRecoveryRuns    prometheus.Counter
	ChaosRecoveryActive  prometheus.Gauge
	RecoveryDurationMs   prometheus.Histogram
	ElectionDurationMs   prometheus.Histogram
	TermSurges           prometheus.Counter
	TermSpread           prometheus.Gauge
}

// NewGatewayMetrics creates and registers all gateway metrics with Prometheus.
func NewGatewayMetrics() *GatewayMetrics {
	m := &GatewayMetrics{
		WebsocketConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_websocket_connections_total",
			Help: "Current number of active WebSocket connections.",
		}),
		StrokesForwarded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_strokes_forwarded_total",
			Help: "Total number of strokes forwarded to the leader.",
		}),
		StrokesBuffered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_strokes_buffered_total",
			Help: "Total number of strokes buffered while no leader was available.",
		}),
		StrokesDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_strokes_dropped_total",
			Help: "Total number of buffered strokes dropped due to timeout.",
		}),
		LeaderChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_leader_changes_total",
			Help: "Total number of leader changes observed.",
		}),
		ChaosEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_chaos_events_total",
			Help: "Total number of chaos events triggered, labelled by mode.",
		}, []string{"mode"}),
		ChaosRecoveryRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_chaos_recovery_runs_total",
			Help: "Total number of chaos recovery runs started.",
		}),
		ChaosRecoveryActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_chaos_recovery_active",
			Help: "Whether a chaos recovery run is currently active (1 active, 0 idle).",
		}),
		RecoveryDurationMs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_chaos_recovery_duration_ms",
			Help:    "End-to-end duration from chaos injection to cluster stability in milliseconds.",
			Buckets: []float64{100, 250, 500, 1000, 2000, 5000, 10000, 15000},
		}),
		ElectionDurationMs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_chaos_election_duration_ms",
			Help:    "Duration from chaos injection to first leader election in milliseconds.",
			Buckets: []float64{50, 100, 200, 300, 500, 800, 1000, 2000},
		}),
		TermSurges: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_term_surge_events_total",
			Help: "Total number of detected term surge anomalies.",
		}),
		TermSpread: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_term_spread",
			Help: "Current term spread among healthy replicas (maxTerm-minTerm).",
		}),
	}

	prometheus.MustRegister(
		m.WebsocketConnections,
		m.StrokesForwarded,
		m.StrokesBuffered,
		m.StrokesDropped,
		m.LeaderChanges,
		m.ChaosEvents,
		m.ChaosRecoveryRuns,
		m.ChaosRecoveryActive,
		m.RecoveryDurationMs,
		m.ElectionDurationMs,
		m.TermSurges,
		m.TermSpread,
	)

	return m
}

// IncrConnections increments the WebSocket connections gauge.
func (m *GatewayMetrics) IncrConnections() {
	m.WebsocketConnections.Inc()
}

// DecrConnections decrements the WebSocket connections gauge.
func (m *GatewayMetrics) DecrConnections() {
	m.WebsocketConnections.Dec()
}

// IncrStrokes increments the strokes forwarded counter.
func (m *GatewayMetrics) IncrStrokes() {
	m.StrokesForwarded.Inc()
}

// IncrBuffered increments the buffered strokes counter.
func (m *GatewayMetrics) IncrBuffered() {
	m.StrokesBuffered.Inc()
}

// IncrDropped increments the dropped strokes counter.
func (m *GatewayMetrics) IncrDropped() {
	m.StrokesDropped.Inc()
}

// IncrLeaderChanges increments the leader changes counter.
func (m *GatewayMetrics) IncrLeaderChanges() {
	m.LeaderChanges.Inc()
}

// IncrChaos increments the chaos events counter for the given mode.
func (m *GatewayMetrics) IncrChaos(mode string) {
	m.ChaosEvents.WithLabelValues(mode).Inc()
}

// IncrRecoveryRuns increments the recovery run count.
func (m *GatewayMetrics) IncrRecoveryRuns() {
	m.ChaosRecoveryRuns.Inc()
}

// SetRecoveryActive sets whether a recovery run is active.
func (m *GatewayMetrics) SetRecoveryActive(active bool) {
	if active {
		m.ChaosRecoveryActive.Set(1)
		return
	}
	m.ChaosRecoveryActive.Set(0)
}

// ObserveRecoveryDuration records full recovery duration in milliseconds.
func (m *GatewayMetrics) ObserveRecoveryDuration(ms float64) {
	m.RecoveryDurationMs.Observe(ms)
}

// ObserveElectionDuration records election duration in milliseconds.
func (m *GatewayMetrics) ObserveElectionDuration(ms float64) {
	m.ElectionDurationMs.Observe(ms)
}

// IncrTermSurges increments detected term surge counter.
func (m *GatewayMetrics) IncrTermSurges() {
	m.TermSurges.Inc()
}

// SetTermSpread updates current spread among healthy replica terms.
func (m *GatewayMetrics) SetTermSpread(spread float64) {
	m.TermSpread.Set(spread)
}
