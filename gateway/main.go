package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"miniraft/gateway/anomaly"
	"miniraft/gateway/chaos"
	"miniraft/gateway/leader"
	"miniraft/gateway/metrics"
	"miniraft/gateway/recovery"
	"miniraft/gateway/sse"
	"miniraft/gateway/ws"
)

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func validateGatewaySecurityConfig(chaosEnabled bool, chaosToken, internalCommitToken string) error {
	if strings.TrimSpace(internalCommitToken) == "" {
		return fmt.Errorf("INTERNAL_COMMIT_TOKEN must be set")
	}
	if chaosEnabled && strings.TrimSpace(chaosToken) == "" {
		return fmt.Errorf("CHAOS_ENABLED=true requires CHAOS_TOKEN")
	}
	return nil
}

func buildLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		zapLevel = zapcore.InfoLevel
	}

	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(cfg),
		zapcore.AddSync(os.Stdout),
		zapLevel,
	)
	return zap.New(core), nil
}

func main() {
	port := getEnv("GATEWAY_PORT", "8080")
	replicaAddrs := getEnv("REPLICA_ADDRS", "replica1:9001,replica2:9001,replica3:9001")
	replicaHTTP := getEnv("REPLICA_HTTP", "http://replica1:8081,http://replica2:8081,http://replica3:8081")
	logLevel := getEnv("LOG_LEVEL", "info")
	chaosEnabled := strings.EqualFold(getEnv("CHAOS_ENABLED", "false"), "true")
	chaosToken := getEnv("CHAOS_TOKEN", "")
	internalCommitToken := getEnv("INTERNAL_COMMIT_TOKEN", "")
	raftNetwork := getEnv("RAFT_NETWORK", "") // Docker network name for partition/heal chaos modes

	if err := validateGatewaySecurityConfig(chaosEnabled, chaosToken, internalCommitToken); err != nil {
		fmt.Fprintf(os.Stderr, "gateway security config invalid: %v\n", err)
		os.Exit(1)
	}

	logger, err := buildLogger(logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// Build replica configs from env vars.
	grpcAddrs := strings.Split(replicaAddrs, ",")
	httpAddrs := strings.Split(replicaHTTP, ",")

	replicas := make([]leader.ReplicaConfig, 0, len(grpcAddrs))
	for i, addr := range grpcAddrs {
		id := fmt.Sprintf("replica%d", i+1)
		httpBase := ""
		if i < len(httpAddrs) {
			httpBase = strings.TrimSpace(httpAddrs[i])
		}
		replicas = append(replicas, leader.ReplicaConfig{
			ID:         id,
			StatusURL:  httpBase + "/status",
			StrokeURL:  httpBase + "/stroke",
			EntriesURL: httpBase + "/entries",
		})
		logger.Info("registered replica",
			zap.String("id", id),
			zap.String("grpc", strings.TrimSpace(addr)),
			zap.String("http", httpBase),
		)
	}

	// Create metrics.
	m := metrics.NewGatewayMetrics()

	// sseHub and wsHub are created below; use pointers so onChange can reference them.
	var sseHub *sse.SSEHub
	var wsHub *ws.WSHub
	var recoveryTracker *recovery.Tracker

	broadcastSSE := func(event string, payload interface{}) {
		if sseHub == nil {
			return
		}
		data, err := json.Marshal(payload)
		if err != nil {
			logger.Warn("failed to marshal SSE payload",
				zap.String("event", event),
				zap.Error(err),
			)
			return
		}
		sseHub.Broadcast(event, string(data))
	}

	// Create leader tracker.
	tracker := leader.NewLeaderTracker(replicas, logger, func(newLeaderID string, term int64) {
		m.IncrLeaderChanges()
		logger.Info("leader changed", zap.String("leader", newLeaderID), zap.Int64("term", term))
		broadcastSSE("leader_elected", map[string]interface{}{
			"leaderId": newLeaderID,
			"term":     term,
		})
		if recoveryTracker != nil {
			recoveryTracker.ObserveLeader(newLeaderID, term, time.Now())
		}
		// Push LEADER_CHANGED to all WebSocket clients so the toolbar updates immediately.
		if wsHub != nil {
			wsHub.BroadcastMessage("LEADER_CHANGED", map[string]interface{}{
				"leaderId": newLeaderID,
				"term":     term,
			})
		}
	})

	// Create SSE hub.
	sseHub = sse.NewSSEHub(tracker, logger)

	// Create WebSocket hub.
	wsHub = ws.NewWSHub(tracker, logger, m)

	// Create recovery tracker and term detector for forensics events.
	recoveryTracker = recovery.NewTracker(logger, m, broadcastSSE, func() recovery.BufferStats {
		stats := wsHub.SnapshotBufferStats()
		return recovery.BufferStats{
			BufferedTotal: stats.BufferedTotal,
			DroppedTotal:  stats.DroppedTotal,
		}
	})
	termDetector := anomaly.NewTermDetector(logger, m, broadcastSSE)

	// Create chaos handler (best-effort; falls back to stub if Docker unavailable).
	var chaosHTTP http.Handler
	if !chaosEnabled {
		logger.Info("chaos endpoint disabled by config")
		chaosHTTP = http.HandlerFunc(chaos.ServeHTTPDisabled)
	} else {
		chaosHandler, err := chaos.NewChaosHandler(
			tracker,
			logger,
			m,
			chaosToken,
			raftNetwork,
			func(target, mode, container string, at time.Time) string {
				if recoveryTracker == nil {
					return ""
				}
				return recoveryTracker.StartRun(target, mode, container, at)
			},
		)
		if err != nil {
			logger.Warn("chaos handler unavailable (Docker socket not accessible), using stub", zap.Error(err))
			chaosHTTP = http.HandlerFunc(chaos.ServeHTTPStub)
		} else {
			chaosHTTP = chaosHandler
		}
	}

	// Build HTTP mux.
	mux := http.NewServeMux()
	mux.Handle("/ws", wsHub)
	mux.Handle("/events/cluster-status", sseHub)
	mux.Handle("/chaos", chaosHTTP)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	})
	mux.HandleFunc("/internal/committed", internalCommittedHandler(
		wsHub,
		logger,
		internalCommitToken,
		tracker.GetLeaderID,
		recoveryTracker.ActiveRunID,
		func(entryTimestampMs int64) {
			recoveryTracker.RecordCommitLatency(entryTimestampMs, time.Now())
		},
	))

	// Start background goroutines.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	tracker.Start(ctx)
	sseHub.StartBroadcasting(ctx)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				statuses := tracker.GetAllStatuses()
				termDetector.Observe(statuses, now)
				recoveryTracker.ObserveStatuses(statuses, now)
			}
		}
	}()

	logger.Info("gateway listening", zap.String("port", port))
	server := &http.Server{
		Addr:    ":" + port,
		Handler: corsMiddleware(mux),
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Fatal("gateway server error", zap.Error(err))
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, shutting down gateway")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("gateway shutdown timed out", zap.Error(err))
	}

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("gateway server exited with error", zap.Error(err))
		}
	default:
	}

	logger.Info("gateway shutdown complete")
}

type wsBroadcaster interface {
	BroadcastMessage(msgType string, payload interface{})
}

// internalCommittedHandler handles POST /internal/committed from replicas.
// The leader calls this whenever an entry is committed, and the gateway
// broadcasts STROKE_COMMITTED to all WebSocket clients.
func internalCommittedHandler(
	hub wsBroadcaster,
	logger *zap.Logger,
	internalCommitToken string,
	currentLeader func() string,
	activeRunID func() string,
	onCommitted func(entryTimestampMs int64),
) http.HandlerFunc {
	type committedStrokeData struct {
		Points []map[string]float64 `json:"points"`
		Colour string               `json:"colour"`
		Width  float64              `json:"width"`
		Tool   string               `json:"tool"`
	}
	type committedEntry struct {
		Index     int64               `json:"index"`
		Term      int64               `json:"term"`
		Type      string              `json:"type"`
		StrokeID  string              `json:"strokeId"`
		UserID    string              `json:"userId"`
		Timestamp int64               `json:"ts"`
		Data      committedStrokeData `json:"data"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if subtle.ConstantTimeCompare(
			[]byte(r.Header.Get("X-Replica-Token")),
			[]byte(internalCommitToken),
		) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var entry committedEntry
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		logger.Debug("internal/committed received",
			zap.String("strokeId", entry.StrokeID),
			zap.String("userId", entry.UserID),
		)

		if onCommitted != nil {
			onCommitted(entry.Timestamp)
		}

		leaderID := ""
		if currentLeader != nil {
			leaderID = currentLeader()
		}

		runID := ""
		if activeRunID != nil {
			runID = activeRunID()
		}

		if entry.Type == "UNDO_COMPENSATION" {
			// Broadcast undo removal to all clients.
			hub.BroadcastMessage("UNDO_COMPENSATION", map[string]interface{}{
				"strokeId":      entry.StrokeID,
				"userId":        entry.UserID,
				"index":         entry.Index,
				"term":          entry.Term,
				"ts":            entry.Timestamp,
				"leaderId":      leaderID,
				"recoveryRunId": runID,
			})
		} else {
			// Build the payload that the frontend's STROKE_COMMITTED handler expects.
			hub.BroadcastMessage("STROKE_COMMITTED", map[string]interface{}{
				"strokeId":      entry.StrokeID,
				"userId":        entry.UserID,
				"points":        entry.Data.Points,
				"colour":        entry.Data.Colour,
				"width":         entry.Data.Width,
				"strokeTool":    entry.Data.Tool,
				"index":         entry.Index,
				"term":          entry.Term,
				"ts":            entry.Timestamp,
				"leaderId":      leaderID,
				"recoveryRunId": runID,
			})
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// corsMiddleware adds permissive CORS headers for development.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
