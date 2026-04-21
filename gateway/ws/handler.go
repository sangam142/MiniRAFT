package ws

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// colourPalette is 16 distinct accessible colours cycled per new connection.
var colourPalette = []string{
	"#E63946", "#F4A261", "#2A9D8F", "#264653",
	"#A8DADC", "#457B9D", "#E9C46A", "#F77F00",
	"#D62828", "#023E8A", "#80B918", "#6A4C93",
	"#FF6B6B", "#4ECDC4", "#45B7D1", "#96CEB4",
}

// WSMessage is the generic envelope for all WebSocket messages.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Client represents a single connected WebSocket client.
type Client struct {
	UserID string
	Colour string
	Conn   *websocket.Conn
	Send   chan []byte
}

// pendingStroke holds a stroke buffered during a leaderless period.
type pendingStroke struct {
	userID    string
	payload   json.RawMessage
	strokeID  string
	kind      string    // "draw" or "undo"
	arrivedAt time.Time // for 2-second timeout tracking
}

// BufferStats reports aggregate buffering behavior for recovery analytics.
type BufferStats struct {
	BufferedTotal uint64
	DroppedTotal  uint64
}

// WSHub manages all WebSocket clients.
type WSHub struct {
	mu          sync.RWMutex
	clients     map[string]*Client // userID -> Client
	colourIndex int
	tracker     *leader.LeaderTracker
	logger      *zap.Logger
	metrics     interface {
		IncrConnections()
		DecrConnections()
		IncrStrokes()
		IncrBuffered()
		IncrDropped()
	}

	// Stroke buffer: holds strokes queued while there is no leader.
	bufMu  sync.Mutex
	buffer []pendingStroke

	// Deduplication cache: recently seen strokeIds to drop client retries.
	seenMu         sync.Mutex
	seenStrokes    map[string]time.Time // strokeId -> forwarded successfully
	pendingStrokes map[string]time.Time // strokeId -> accepted but not yet forwarded

	bufferedTotal atomic.Uint64
	droppedTotal  atomic.Uint64
}

// NewWSHub creates a new WSHub.
func NewWSHub(tracker *leader.LeaderTracker, logger *zap.Logger, metrics interface {
	IncrConnections()
	DecrConnections()
	IncrStrokes()
	IncrBuffered()
	IncrDropped()
}) *WSHub {
	h := &WSHub{
		clients:        make(map[string]*Client),
		tracker:        tracker,
		logger:         logger,
		metrics:        metrics,
		seenStrokes:    make(map[string]time.Time),
		pendingStrokes: make(map[string]time.Time),
	}
	go h.drainBuffer()
	// Expire seen strokeIds older than 60 seconds every 30 seconds.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			h.seenMu.Lock()
			cutoff := time.Now().Add(-60 * time.Second)
			for id, t := range h.seenStrokes {
				if t.Before(cutoff) {
					delete(h.seenStrokes, id)
				}
			}
			pendingCutoff := time.Now().Add(-10 * time.Second)
			for id, t := range h.pendingStrokes {
				if t.Before(pendingCutoff) {
					delete(h.pendingStrokes, id)
				}
			}
			h.seenMu.Unlock()
		}
	}()
	return h
}

func (h *WSHub) reserveStrokeID(strokeID string) bool {
	if strokeID == "" {
		return true
	}
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	if _, seen := h.seenStrokes[strokeID]; seen {
		return false
	}
	if _, pending := h.pendingStrokes[strokeID]; pending {
		return false
	}
	h.pendingStrokes[strokeID] = time.Now()
	return true
}

func (h *WSHub) markStrokeForwarded(strokeID string) {
	if strokeID == "" {
		return
	}
	h.seenMu.Lock()
	delete(h.pendingStrokes, strokeID)
	h.seenStrokes[strokeID] = time.Now()
	h.seenMu.Unlock()
}

func (h *WSHub) releaseStrokeReservation(strokeID string) {
	if strokeID == "" {
		return
	}
	h.seenMu.Lock()
	delete(h.pendingStrokes, strokeID)
	h.seenMu.Unlock()
}

func (h *WSHub) enqueueBufferedStroke(p pendingStroke) {
	h.bufMu.Lock()
	h.buffer = append(h.buffer, p)
	h.bufMu.Unlock()
}

// ServeHTTP upgrades an HTTP connection to WebSocket and registers the client.
func (h *WSHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("websocket upgrade failed", zap.Error(err))
		return
	}

	userID := uuid.New().String()

	h.mu.Lock()
	colour := colourPalette[h.colourIndex%len(colourPalette)]
	h.colourIndex++
	client := &Client{
		UserID: userID,
		Colour: colour,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}
	h.clients[userID] = client
	h.mu.Unlock()

	h.logger.Info("websocket client connected", zap.String("userID", userID), zap.String("colour", colour))
	if h.metrics != nil {
		h.metrics.IncrConnections()
	}

	// Send USER_COLOR_ASSIGNED.
	h.sendJSON(client, "USER_COLOR_ASSIGNED", map[string]string{
		"userId": userID,
		"colour": colour,
	})

	// Send CANVAS_SYNC: fetch all committed entries from the leader.
	go h.sendCanvasSync(client)

	// Start write goroutine.
	go h.writePump(client)

	// Read pump runs in the current goroutine.
	h.readPump(client)
}

// sendCanvasSync fetches committed log entries from the leader and sends them to the client.
func (h *WSHub) sendCanvasSync(client *Client) {
	cfg, ok := h.tracker.GetLeaderConfig()
	if !ok {
		h.logger.Warn("CANVAS_SYNC: no leader available", zap.String("userID", client.UserID))
		return
	}

	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Get(cfg.EntriesURL)
	if err != nil {
		h.logger.Warn("CANVAS_SYNC: failed to fetch entries",
			zap.String("url", cfg.EntriesURL),
			zap.Error(err),
		)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Warn("CANVAS_SYNC: failed to read entries body", zap.Error(err))
		return
	}

	// Parse entries to normalise the shape for the frontend.
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(body, &rawEntries); err != nil {
		h.logger.Warn("CANVAS_SYNC: failed to parse entries", zap.Error(err))
		return
	}

	h.sendJSON(client, "CANVAS_SYNC", map[string]interface{}{
		"entries": rawEntries,
	})

	h.logger.Debug("CANVAS_SYNC sent",
		zap.String("userID", client.UserID),
		zap.Int("entries", len(rawEntries)),
	)
}

// readPump reads messages from the client until the connection closes.
func (h *WSHub) readPump(client *Client) {
	defer func() {
		h.mu.Lock()
		delete(h.clients, client.UserID)
		h.mu.Unlock()
		close(client.Send)
		client.Conn.Close()
		if h.metrics != nil {
			h.metrics.DecrConnections()
		}
		h.logger.Info("websocket client disconnected", zap.String("userID", client.UserID))
	}()

	client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.Conn.SetPongHandler(func(string) error {
		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				h.logger.Warn("websocket unexpected close", zap.String("userID", client.UserID), zap.Error(err))
			}
			return
		}

		var msg WSMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			h.logger.Warn("failed to unmarshal ws message", zap.String("userID", client.UserID), zap.Error(err))
			continue
		}

		h.handleMessage(client, msg)
	}
}

// writePump writes messages from the Send channel to the WebSocket connection.
func (h *WSHub) writePump(client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		client.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-client.Send:
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := client.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				h.logger.Warn("websocket write error", zap.String("userID", client.UserID), zap.Error(err))
				return
			}
		case <-ticker.C:
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage processes an incoming WebSocket message from a client.
func (h *WSHub) handleMessage(client *Client, msg WSMessage) {
	switch msg.Type {
	case "STROKE_DRAW":
		h.logger.Info("STROKE_DRAW received",
			zap.String("userID", client.UserID),
		)
		go h.forwardStrokeToLeader(client.UserID, msg.Payload, false, time.Now())
		if h.metrics != nil {
			h.metrics.IncrStrokes()
		}

	case "STROKE_UNDO":
		h.logger.Info("STROKE_UNDO received", zap.String("userID", client.UserID))
		go h.forwardUndoToLeader(client.UserID, msg.Payload)

	default:
		h.logger.Warn("unknown ws message type",
			zap.String("userID", client.UserID),
			zap.String("type", msg.Type),
		)
	}
}

// drainBuffer retries buffered strokes every 100ms. Strokes older than 2 seconds
// are discarded and an ERROR message is sent to the originating client.
func (h *WSHub) drainBuffer() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		h.bufMu.Lock()
		if len(h.buffer) == 0 {
			h.bufMu.Unlock()
			continue
		}

		now := time.Now()
		_, hasLeader := h.tracker.GetLeaderConfig()

		var ready []pendingStroke
		var remaining []pendingStroke

		for _, p := range h.buffer {
			if now.Sub(p.arrivedAt) > 2*time.Second {
				// Expired — notify client and discard.
				h.logger.Warn("stroke buffer timeout, dropping stroke",
					zap.String("userID", p.userID),
					zap.Duration("buffered", now.Sub(p.arrivedAt)),
				)
				h.droppedTotal.Add(1)
				if h.metrics != nil {
					h.metrics.IncrDropped()
				}
				if p.kind == "draw" && p.strokeID != "" {
					h.releaseStrokeReservation(p.strokeID)
				}
				go h.SendToClient(p.userID, "ERROR", map[string]string{
					"message": "stroke dropped: no leader available after 2 seconds",
				})
			} else if hasLeader {
				ready = append(ready, p)
			} else {
				remaining = append(remaining, p)
			}
		}
		h.buffer = remaining
		h.bufMu.Unlock()

		if len(ready) > 0 {
			h.logger.Info("draining stroke buffer", zap.Int("count", len(ready)))
			for _, p := range ready {
				if p.kind == "undo" {
					go h.forwardUndoToLeader(p.userID, p.payload)
				} else {
					go h.forwardStrokeToLeader(p.userID, p.payload, true, p.arrivedAt)
				}
			}
		}
	}
}

// extractStrokeID parses strokeId from a raw JSON stroke payload.
func extractStrokeID(payload json.RawMessage) string {
	var m struct {
		StrokeID string `json:"strokeId"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return m.StrokeID
}

// forwardStrokeToLeader sends a stroke to the leader's POST /stroke endpoint.
// Retries once if the leader returns 503 (stepped down). Buffers if no leader.
// Deduplicates by strokeId to drop client retries.
func (h *WSHub) forwardStrokeToLeader(userID string, payload json.RawMessage, fromBuffer bool, arrivedAt time.Time) {
	strokeID := extractStrokeID(payload)
	if strokeID != "" && !fromBuffer {
		if !h.reserveStrokeID(strokeID) {
			h.logger.Debug("duplicate strokeId dropped",
				zap.String("strokeId", strokeID),
				zap.String("userID", userID),
			)
			return
		}
	}

	body, err := json.Marshal(map[string]interface{}{
		"userId":  userID,
		"payload": payload,
	})
	if err != nil {
		h.logger.Error("failed to marshal stroke body", zap.Error(err))
		h.releaseStrokeReservation(strokeID)
		return
	}

	shouldBuffer := false

	for attempt := 0; attempt < 2; attempt++ {
		cfg, ok := h.tracker.GetLeaderConfig()
		if !ok {
			h.logger.Warn("no leader — buffering stroke until election completes", zap.String("userID", userID))
			shouldBuffer = true
			break
		}

		httpClient := &http.Client{Timeout: 5 * time.Second}
		resp, err := httpClient.Post(cfg.StrokeURL, "application/json", bytes.NewReader(body))
		if err != nil {
			h.logger.Warn("failed to forward stroke to leader",
				zap.String("leaderID", cfg.ID),
				zap.Error(err),
			)
			shouldBuffer = true
			break
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()

		if resp.StatusCode == http.StatusServiceUnavailable && attempt == 0 {
			// Leader stepped down — wait briefly for tracker to detect new leader, then retry.
			h.logger.Warn("leader returned 503, retrying with fresh leader",
				zap.String("staleLeader", cfg.ID),
			)
			time.Sleep(60 * time.Millisecond)
			continue
		}

		if resp.StatusCode == http.StatusServiceUnavailable {
			shouldBuffer = true
			break
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			h.logger.Warn("leader rejected stroke",
				zap.String("leaderID", cfg.ID),
				zap.Int("status", resp.StatusCode),
			)
			h.releaseStrokeReservation(strokeID)
			go h.SendToClient(userID, "ERROR", map[string]string{
				"message": "stroke rejected by leader",
			})
			return
		}

		h.logger.Debug("stroke forwarded to leader",
			zap.String("leaderID", cfg.ID),
			zap.Int("status", resp.StatusCode),
		)
		h.markStrokeForwarded(strokeID)
		return
	}

	if shouldBuffer {
		h.bufferedTotal.Add(1)
		if h.metrics != nil {
			h.metrics.IncrBuffered()
		}
		h.enqueueBufferedStroke(pendingStroke{
			userID:    userID,
			payload:   payload,
			strokeID:  strokeID,
			kind:      "draw",
			arrivedAt: arrivedAt,
		})
		return
	}

	h.releaseStrokeReservation(strokeID)
}

// forwardUndoToLeader sends a STROKE_UNDO to the leader's POST /undo endpoint.
func (h *WSHub) forwardUndoToLeader(userID string, payload json.RawMessage) {
	cfg, ok := h.tracker.GetLeaderConfig()
	if !ok {
		h.logger.Warn("no leader — buffering undo until election completes", zap.String("userID", userID))
		h.bufMu.Lock()
		h.buffer = append(h.buffer, pendingStroke{userID: userID, payload: payload, kind: "undo", arrivedAt: time.Now()})
		h.bufMu.Unlock()
		return
	}

	body, err := json.Marshal(map[string]interface{}{
		"userId":  userID,
		"payload": payload,
	})
	if err != nil {
		h.logger.Error("failed to marshal undo body", zap.Error(err))
		return
	}

	undoURL := cfg.StrokeURL[:len(cfg.StrokeURL)-len("stroke")] + "undo"
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Post(undoURL, "application/json", bytes.NewReader(body))
	if err != nil {
		h.logger.Warn("failed to forward undo to leader",
			zap.String("leaderID", cfg.ID),
			zap.Error(err),
		)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
}

// sendJSON serialises payload as a WSMessage and enqueues it for the client.
func (h *WSHub) sendJSON(client *Client, msgType string, payload interface{}) {
	raw, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("failed to marshal payload", zap.String("type", msgType), zap.Error(err))
		return
	}
	envelope := WSMessage{Type: msgType, Payload: json.RawMessage(raw)}
	data, err := json.Marshal(envelope)
	if err != nil {
		h.logger.Error("failed to marshal envelope", zap.Error(err))
		return
	}
	select {
	case client.Send <- data:
	default:
		h.logger.Warn("client send buffer full, dropping message",
			zap.String("userID", client.UserID),
			zap.String("type", msgType),
		)
	}
}

// BroadcastMessage serialises a WSMessage and sends it to all connected clients.
func (h *WSHub) BroadcastMessage(msgType string, payload interface{}) {
	raw, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("failed to marshal broadcast payload", zap.String("type", msgType), zap.Error(err))
		return
	}
	envelope := WSMessage{Type: msgType, Payload: json.RawMessage(raw)}
	data, err := json.Marshal(envelope)
	if err != nil {
		h.logger.Error("failed to marshal broadcast envelope", zap.Error(err))
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.clients {
		select {
		case client.Send <- data:
		default:
			h.logger.Warn("broadcast: client buffer full, skipping",
				zap.String("userID", client.UserID),
			)
		}
	}
}

// SendToClient sends a unicast message to a specific client by userID.
func (h *WSHub) SendToClient(userID string, msgType string, payload interface{}) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()
	if !ok {
		h.logger.Warn("SendToClient: client not found", zap.String("userID", userID))
		return
	}
	h.sendJSON(client, msgType, payload)
}

// SnapshotBufferStats returns aggregate buffered and dropped counters.
func (h *WSHub) SnapshotBufferStats() BufferStats {
	return BufferStats{
		BufferedTotal: h.bufferedTotal.Load(),
		DroppedTotal:  h.droppedTotal.Load(),
	}
}
