package chaos

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChaosHandlerRejectsInvalidToken(t *testing.T) {
	h := &ChaosHandler{token: "expected-token"}

	req := httptest.NewRequest(
		http.MethodPost,
		"/chaos",
		strings.NewReader(`{"target":"random","mode":"random"}`),
	)
	req.Header.Set("X-Chaos-Token", "wrong-token")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestChaosHandlerRejectsMissingToken(t *testing.T) {
	h := &ChaosHandler{token: "expected-token"}

	// No X-Chaos-Token header set at all.
	req := httptest.NewRequest(
		http.MethodPost,
		"/chaos",
		strings.NewReader(`{"target":"random","mode":"random"}`),
	)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d for missing header, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestChaosHandlerPartitionRequiresRaftNetwork(t *testing.T) {
	// raftNetwork is empty — partition mode must be rejected with 400.
	h := &ChaosHandler{token: "", raftNetwork: ""}

	req := httptest.NewRequest(
		http.MethodPost,
		"/chaos",
		strings.NewReader(`{"target":"replica1","mode":"partition"}`),
	)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when RAFT_NETWORK unset, got %d", rr.Code)
	}
}
