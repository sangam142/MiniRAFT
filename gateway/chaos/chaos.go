package chaos

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	"miniraft/gateway/leader"
)

// dockerHTTP is a minimal Docker API client that talks to the Docker daemon
// over the Unix socket using only stdlib net/http. It replaces
// github.com/docker/docker/client, which pulls in otelhttp (requires Go 1.25).
type dockerHTTP struct {
	client *http.Client
}

func newDockerHTTP() *dockerHTTP {
	return &dockerHTTP{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
				},
			},
		},
	}
}

// ContainerStop sends POST /containers/{id}/stop?t={timeout} to the Docker daemon.
func (d *dockerHTTP) ContainerStop(ctx context.Context, containerID string, timeoutSec int) error {
	url := fmt.Sprintf("http://localhost/containers/%s/stop?t=%d", containerID, timeoutSec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != 304 {
		return fmt.Errorf("docker ContainerStop: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ContainerKill sends POST /containers/{id}/kill?signal={sig} to the Docker daemon.
func (d *dockerHTTP) ContainerKill(ctx context.Context, containerID string, signal string) error {
	url := fmt.Sprintf("http://localhost/containers/%s/kill?signal=%s", containerID, signal)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker ContainerKill: HTTP %d", resp.StatusCode)
	}
	return nil
}

// NetworkDisconnect severs a container from the named Docker network without
// stopping it.  This simulates a network partition: the node stays running but
// loses all peer connectivity, triggering split-brain behaviour.
func (d *dockerHTTP) NetworkDisconnect(ctx context.Context, networkName, containerID string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"Container": containerID,
		"Force":     false,
	})
	reqURL := fmt.Sprintf("http://localhost/networks/%s/disconnect", url.PathEscape(networkName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker NetworkDisconnect: HTTP %d", resp.StatusCode)
	}
	return nil
}

// NetworkConnect re-attaches a previously partitioned container to the named
// Docker network, healing the simulated partition.
func (d *dockerHTTP) NetworkConnect(ctx context.Context, networkName, containerID string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"Container":      containerID,
		"EndpointConfig": map[string]interface{}{},
	})
	reqURL := fmt.Sprintf("http://localhost/networks/%s/connect", url.PathEscape(networkName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker NetworkConnect: HTTP %d", resp.StatusCode)
	}
	return nil
}

// containerForService queries the Docker daemon for the container that has the
// compose label com.docker.compose.service=<service>.  It returns the container
// ID (for API calls) and the human-readable name (for responses), so that the
// chaos handler never needs to know the compose project name or naming scheme.
func (d *dockerHTTP) containerForService(ctx context.Context, service string) (id, name string, err error) {
	filter := fmt.Sprintf(`{"label":["com.docker.compose.service=%s"]}`, service)
	reqURL := "http://localhost/containers/json?filters=" + url.QueryEscape(filter)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return "", "", err
	}
	if len(containers) == 0 {
		return "", "", fmt.Errorf("no container found for compose service %q", service)
	}
	cid := containers[0].ID
	cname := containers[0].Names[0] // Docker prefixes names with "/"
	cname = strings.TrimPrefix(cname, "/")
	return cid, cname, nil
}

// ChaosRequest is the JSON body for POST /chaos.
type ChaosRequest struct {
	Target string `json:"target"` // "random"|"replica1"|"replica2"|"replica3"|"replica4"
	Mode   string `json:"mode"`   // "graceful"|"hard"|"random"
}

// ChaosResponse is returned after a chaos action.
type ChaosResponse struct {
	Killed    string `json:"killed"`
	Mode      string `json:"mode"`
	Timestamp int64  `json:"timestamp"`
	RunID     string `json:"runId,omitempty"`
}

// ChaosHandler executes chaos actions against Docker containers.
type ChaosHandler struct {
	docker      *dockerHTTP
	tracker     *leader.LeaderTracker
	logger      *zap.Logger
	metrics     interface{ IncrChaos(mode string) }
	token       string
	raftNetwork string // Docker network name for partition/heal (RAFT_NETWORK env var)
	onExecute   func(target, mode, container string, at time.Time) string
}

// NewChaosHandler creates a new ChaosHandler, connecting to the Docker daemon.
func NewChaosHandler(
	tracker *leader.LeaderTracker,
	logger *zap.Logger,
	metrics interface{ IncrChaos(mode string) },
	token string,
	raftNetwork string,
	onExecute func(target, mode, container string, at time.Time) string,
) (*ChaosHandler, error) {
	return &ChaosHandler{
		docker:      newDockerHTTP(),
		tracker:     tracker,
		logger:      logger,
		metrics:     metrics,
		token:       token,
		raftNetwork: raftNetwork,
		onExecute:   onExecute,
	}, nil
}

// jsonError writes a JSON {"error":"..."} response with the given status code.
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ServeHTTP handles POST /chaos requests.
func (h *ChaosHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Chaos-Token")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.token != "" {
		if subtle.ConstantTimeCompare(
			[]byte(r.Header.Get("X-Chaos-Token")),
			[]byte(h.token),
		) != 1 {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var req ChaosRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Resolve target
	target := req.Target
	if target == "random" || target == "" {
		statuses := h.tracker.GetAllStatuses()
		var healthy []string
		for _, s := range statuses {
			if s.Healthy {
				healthy = append(healthy, s.ReplicaID)
			}
		}
		if len(healthy) == 0 {
			for _, s := range statuses {
				healthy = append(healthy, s.ReplicaID)
			}
		}
		if len(healthy) > 0 {
			target = healthy[rand.Intn(len(healthy))]
		} else {
			jsonError(w, "no replicas available", http.StatusServiceUnavailable)
			return
		}
	}

	// Resolve mode
	mode := req.Mode
	if mode == "random" || mode == "" {
		choices := []string{"graceful", "hard"}
		if h.raftNetwork != "" {
			choices = append(choices, "partition")
		}
		mode = choices[rand.Intn(len(choices))]
	}

	ctx := r.Context()

	// Resolve the compose service name (e.g. "replica1") to the actual
	// container ID and human-readable name (e.g. "miniraft-replica1-1").
	// This avoids hardcoding the project name or container naming scheme.
	containerID, containerName, resolveErr := h.docker.containerForService(ctx, target)
	if resolveErr != nil {
		h.logger.Error("could not resolve container for service",
			zap.String("service", target),
			zap.Error(resolveErr),
		)
		jsonError(w, "container not found for "+target+": "+resolveErr.Error(), http.StatusNotFound)
		return
	}

	// Partition and heal modes require the raft network name to be configured.
	if (mode == "partition" || mode == "heal") && h.raftNetwork == "" {
		jsonError(w, "partition/heal modes require RAFT_NETWORK to be set", http.StatusBadRequest)
		return
	}

	var actionErr error

	switch mode {
	case "graceful":
		actionErr = h.docker.ContainerStop(ctx, containerID, 5)
	case "hard":
		actionErr = h.docker.ContainerKill(ctx, containerID, "SIGKILL")
	case "partition":
		// Disconnect from Raft network without stopping the container.
		// The node stays alive but loses all peer connectivity — triggering
		// elections the partitioned node can never win (no majority).
		actionErr = h.docker.NetworkDisconnect(ctx, h.raftNetwork, containerID)
	case "heal":
		// Reconnect the container to the Raft network, healing the partition.
		// The node rejoins as follower, discovers the higher term, and syncs.
		actionErr = h.docker.NetworkConnect(ctx, h.raftNetwork, containerID)
	default:
		jsonError(w, "unknown mode: "+mode, http.StatusBadRequest)
		return
	}

	if actionErr != nil {
		h.logger.Error("chaos action failed",
			zap.String("container", containerName),
			zap.String("mode", mode),
			zap.Error(actionErr),
		)
		jsonError(w, "chaos action failed: "+actionErr.Error(), http.StatusInternalServerError)
		return
	}

	h.logger.Info("chaos action executed",
		zap.String("container", containerName),
		zap.String("service", target),
		zap.String("mode", mode),
	)

	if h.metrics != nil {
		h.metrics.IncrChaos(mode)
	}

	runID := ""
	if h.onExecute != nil {
		runID = h.onExecute(target, mode, containerName, time.Now())
	}

	resp := ChaosResponse{
		Killed:    containerName,
		Mode:      mode,
		Timestamp: time.Now().UnixMilli(),
		RunID:     runID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// ServeHTTPStub is a no-op handler used when Docker is unavailable.
func ServeHTTPStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "chaos endpoint unavailable: Docker socket not accessible",
	})
}

// ServeHTTPDisabled is used when chaos is intentionally disabled by config.
func ServeHTTPDisabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "chaos endpoint disabled",
	})
}
