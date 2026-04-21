package status

import (
	"encoding/json"
	"net/http"

	"miniraft/replica/raft"
)

// StatusHandler serves HTTP status and health endpoints.
type StatusHandler struct {
	node *raft.RaftNode
}

// NewStatusHandler constructs a StatusHandler backed by the given RaftNode.
func NewStatusHandler(node *raft.RaftNode) *StatusHandler {
	return &StatusHandler{node: node}
}

// ServeHealth handles GET /health — returns {"ok":true}.
func (h *StatusHandler) ServeHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
}

// ServeStatus handles GET /status — returns the node's current NodeStatus as JSON.
func (h *StatusHandler) ServeStatus(w http.ResponseWriter, r *http.Request) {
	status := h.node.GetStatus()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status) //nolint:errcheck
}
