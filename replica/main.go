package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"

	rafflog "miniraft/replica/log"
	"miniraft/replica/metrics"
	proto "miniraft/replica/proto"
	"miniraft/replica/raft"
	"miniraft/replica/status"
)

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func parsePeerAddrs(peersRaw string) ([]string, error) {
	if strings.TrimSpace(peersRaw) == "" {
		return nil, nil
	}

	parts := strings.Split(peersRaw, ",")
	peers := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))

	for _, raw := range parts {
		peer := strings.TrimSpace(raw)
		if peer == "" {
			continue
		}

		host, port, err := net.SplitHostPort(peer)
		if err != nil || strings.TrimSpace(host) == "" {
			return nil, fmt.Errorf("invalid peer address %q: expected host:port", peer)
		}

		portNum, err := strconv.Atoi(port)
		if err != nil || portNum < 1 || portNum > 65535 {
			return nil, fmt.Errorf("invalid peer address %q: invalid port", peer)
		}

		if _, exists := seen[peer]; exists {
			return nil, fmt.Errorf("duplicate peer address %q", peer)
		}
		seen[peer] = struct{}{}
		peers = append(peers, peer)
	}

	return peers, nil
}

func buildLogger(dataDir string, level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		zapLevel = zapcore.InfoLevel
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir dataDir: %w", err)
	}

	logFile, err := os.OpenFile(
		filepath.Join(dataDir, "app.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0644,
	)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "ts"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	fileCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(logFile),
		zapLevel,
	)
	stdoutCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		zapLevel,
	)

	return zap.New(zapcore.NewTee(fileCore, stdoutCore)), nil
}

func main() {
	replicaID := getEnv("REPLICA_ID", "replica1")
	peersRaw := getEnv("PEERS", "")
	grpcPort := getEnv("GRPC_PORT", "9001")
	httpPort := getEnv("HTTP_PORT", "8081")
	dataDir := getEnv("DATA_DIR", "/data")
	logLevel := getEnv("LOG_LEVEL", "info")

	peers, err := parsePeerAddrs(peersRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid PEERS configuration: %v\n", err)
		os.Exit(1)
	}

	// 1. Init logger.
	logger, err := buildLogger(dataDir, logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// 2. Create WAL.
	wal, err := rafflog.NewWAL(dataDir, logger)
	if err != nil {
		logger.Fatal("failed to create WAL", zap.Error(err))
	}
	defer wal.Close() //nolint:errcheck

	// 3. Replay WAL to restore persisted state.
	walState, err := wal.Replay()
	if err != nil {
		logger.Fatal("failed to replay WAL", zap.Error(err))
	}
	logger.Info("WAL replayed",
		zap.Int64("term", walState.Term),
		zap.String("votedFor", walState.VotedFor),
		zap.Int("entries", len(walState.Entries)),
		zap.Int64("commitIndex", walState.CommitIndex),
	)

	// 4. Create metrics.
	m := metrics.NewReplicaMetrics(replicaID)

	// 5. Create RaftLog and load from WAL.
	raftLog := rafflog.NewRaftLog(wal, logger, m)
	if err := raftLog.LoadFromWAL(); err != nil {
		logger.Fatal("failed to load log from WAL", zap.Error(err))
	}

	// 6. Create RaftNode and restore persisted term/votedFor.
	node := raft.NewRaftNode(replicaID, peers, raftLog, wal, logger, m)
	node.RestoreState(walState.Term, walState.VotedFor)

	// 7. Dial peers (needed before SyncFromPeers and Start).
	node.Dial()

	// 7b. Attempt catch-up sync from peers (best-effort; safe to fail).
	node.SyncFromPeers()

	// 8. Wire commit callback: notify gateway on every committed entry.
	gatewayURL := getEnv("GATEWAY_URL", "")
	internalCommitToken := getEnv("INTERNAL_COMMIT_TOKEN", "")
	if gatewayURL != "" {
		if strings.TrimSpace(internalCommitToken) == "" {
			logger.Fatal("INTERNAL_COMMIT_TOKEN must be set when GATEWAY_URL is configured")
		}
		node.SetOnCommit(func(entry rafflog.LogEntry) {
			notifyGatewayCommitted(gatewayURL, internalCommitToken, entry, logger)
		})
	}

	// 8. Create gRPC server.
	rpcServer := raft.NewRaftRPCServer(node, logger)
	grpcServer := grpc.NewServer()
	proto.RegisterRaftServiceServer(grpcServer, rpcServer)

	// 9. Start gRPC server.
	grpcLis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		logger.Fatal("failed to listen on gRPC port", zap.String("port", grpcPort), zap.Error(err))
	}
	go func() {
		logger.Info("gRPC server listening", zap.String("port", grpcPort))
		if err := grpcServer.Serve(grpcLis); err != nil {
			logger.Fatal("gRPC server error", zap.Error(err))
		}
	}()

	// 10. Build HTTP mux.
	statusHandler := status.NewStatusHandler(node)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", statusHandler.ServeHealth)
	mux.HandleFunc("/status", statusHandler.ServeStatus)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/stroke", strokeHandler(node, raftLog, logger))
	mux.HandleFunc("/undo", undoHandler(node, logger))
	mux.HandleFunc("/entries", entriesHandler(raftLog, logger))

	httpServer := &http.Server{
		Addr:    ":" + httpPort,
		Handler: mux,
	}

	// 11. Start RaftNode.
	node.Start()

	// 12. Start HTTP server in background.
	logger.Info("replica started",
		zap.String("replicaId", replicaID),
		zap.String("grpcPort", grpcPort),
		zap.String("httpPort", httpPort),
		zap.String("dataDir", dataDir),
	)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	// 13. Wait for SIGTERM or SIGINT then shut down gracefully.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	logger.Info("shutdown signal received, draining in-flight RPCs...")
	node.Stop()

	// Stop accepting new gRPC calls and wait for in-flight ones to complete.
	grpcServer.GracefulStop()

	// Shutdown the HTTP server with a 5-second deadline.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutCtx); err != nil {
		logger.Warn("HTTP server shutdown timed out", zap.Error(err))
	}

	logger.Info("replica shutdown complete")
}

// strokeRequest is the JSON body for POST /stroke.
type strokeRequest struct {
	UserID  string          `json:"userId"`
	Payload json.RawMessage `json:"payload"`
}

// strokePayload is the inner payload from the gateway.
type strokePayload struct {
	StrokeID   string          `json:"strokeId"`
	Points     []rafflog.Point `json:"points"`
	Colour     string          `json:"colour"`
	Width      float64         `json:"width"`
	StrokeTool string          `json:"strokeTool"`
}

// strokeHandler handles POST /stroke — appends the stroke to the RAFT log and
// blocks until it is committed by a majority.
func strokeHandler(node *raft.RaftNode, raftLog *rafflog.RaftLog, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		w.Header().Set("Content-Type", "application/json")

		if node.GetState() != raft.Leader {
			leaderID := node.GetLeaderID()
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"error":    "not leader",
				"leaderId": leaderID,
			})
			return
		}

		var req strokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var sp strokePayload
		if err := json.Unmarshal(req.Payload, &sp); err != nil {
			http.Error(w, "invalid payload: "+err.Error(), http.StatusBadRequest)
			return
		}

		tool := sp.StrokeTool
		if tool == "" {
			tool = "pen"
		}

		entry := rafflog.LogEntry{
			Type:      rafflog.EntryTypeStroke,
			StrokeID:  sp.StrokeID,
			UserID:    req.UserID,
			Timestamp: time.Now().UnixMilli(),
			Data: rafflog.StrokeData{
				Points: sp.Points,
				Colour: sp.Colour,
				Width:  sp.Width,
				Tool:   tool,
			},
		}

		committed, err := node.Replicate(entry, 3*time.Second)
		if err != nil {
			logger.Warn("Replicate failed", zap.Error(err))
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"error": err.Error(),
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(committed) //nolint:errcheck
	}
}

// undoRequest is the JSON body for POST /undo.
type undoRequest struct {
	UserID  string          `json:"userId"`
	Payload json.RawMessage `json:"payload"`
}

// undoPayload is the inner payload from the gateway.
type undoPayload struct {
	StrokeID string `json:"strokeId"`
}

// undoHandler handles POST /undo — creates an UNDO_COMPENSATION entry and replicates it.
func undoHandler(node *raft.RaftNode, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		w.Header().Set("Content-Type", "application/json")

		if node.GetState() != raft.Leader {
			leaderID := node.GetLeaderID()
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"error":    "not leader",
				"leaderId": leaderID,
			})
			return
		}

		var req undoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var up undoPayload
		if err := json.Unmarshal(req.Payload, &up); err != nil || up.StrokeID == "" {
			http.Error(w, "invalid payload: missing strokeId", http.StatusBadRequest)
			return
		}

		entry := rafflog.LogEntry{
			Type:      rafflog.EntryTypeUndo,
			StrokeID:  up.StrokeID,
			UserID:    req.UserID,
			Timestamp: time.Now().UnixMilli(),
		}

		committed, err := node.Replicate(entry, 3*time.Second)
		if err != nil {
			logger.Warn("undo Replicate failed", zap.Error(err))
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"error": err.Error(),
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(committed) //nolint:errcheck
	}
}

// notifyGatewayCommitted posts a committed log entry to the gateway.
func notifyGatewayCommitted(gatewayURL string, internalCommitToken string, entry rafflog.LogEntry, logger *zap.Logger) {
	body, err := json.Marshal(entry)
	if err != nil {
		logger.Error("failed to marshal committed entry", zap.Error(err))
		return
	}

	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(
		http.MethodPost,
		gatewayURL+"/internal/committed",
		strings.NewReader(string(body)),
	)
	if err != nil {
		logger.Warn("failed to build gateway commit request", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Replica-Token", internalCommitToken)

	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("failed to notify gateway", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
}

// entriesHandler handles GET /entries — returns all committed log entries as JSON.
func entriesHandler(raftLog *rafflog.RaftLog, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		commitIndex := raftLog.GetCommitIndex()
		allEntries := raftLog.AllEntries()

		var committed []rafflog.LogEntry
		for _, e := range allEntries {
			if e.Index <= commitIndex {
				committed = append(committed, e)
			}
		}
		if committed == nil {
			committed = []rafflog.LogEntry{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(committed) //nolint:errcheck
	}
}
