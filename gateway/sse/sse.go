package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

// SSEHub manages Server-Sent Events connections and broadcasts cluster status.
type SSEHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
	logger  *zap.Logger
	tracker *leader.LeaderTracker
}

// NewSSEHub creates a new SSEHub.
func NewSSEHub(tracker *leader.LeaderTracker, logger *zap.Logger) *SSEHub {
	return &SSEHub{
		clients: make(map[chan string]struct{}),
		logger:  logger,
		tracker: tracker,
	}
}

// ServeHTTP handles GET /events/cluster-status as a Server-Sent Events stream.
func (h *SSEHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Handle CORS preflight
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Ensure the response writer supports flushing
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Create a channel for this client
	ch := make(chan string, 64)

	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	h.logger.Info("SSE client connected", zap.String("remote", r.RemoteAddr))

	// Flush headers immediately
	flusher.Flush()

	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
		close(ch)
		h.logger.Info("SSE client disconnected", zap.String("remote", r.RemoteAddr))
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, msg)
			flusher.Flush()
		}
	}
}

// Broadcast sends an SSE event to all connected clients.
func (h *SSEHub) Broadcast(eventName, data string) {
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventName, data)

	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			// Client channel full — skip to avoid blocking
		}
	}
}

// clusterStatusPayload is used for the cluster_status SSE event.
type clusterStatusPayload struct {
	Replicas []leader.NodeStatus `json:"replicas"`
}

// StartBroadcasting starts a goroutine that pushes cluster_status every 500ms.
func (h *SSEHub) StartBroadcasting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				statuses := h.tracker.GetAllStatuses()
				payload := clusterStatusPayload{Replicas: statuses}
				data, err := json.Marshal(payload)
				if err != nil {
					h.logger.Error("failed to marshal cluster status", zap.Error(err))
					continue
				}
				h.Broadcast("cluster_status", string(data))
			}
		}
	}()
}
