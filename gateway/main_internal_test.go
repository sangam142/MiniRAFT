package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

type broadcastCall struct {
	msgType string
	payload interface{}
}

type mockBroadcaster struct {
	calls []broadcastCall
}

func (m *mockBroadcaster) BroadcastMessage(msgType string, payload interface{}) {
	m.calls = append(m.calls, broadcastCall{msgType: msgType, payload: payload})
}

func TestInternalCommittedHandlerRejectsUnauthorized(t *testing.T) {
	mock := &mockBroadcaster{}
	h := internalCommittedHandler(mock, zap.NewNop(), "good-token", nil, nil, nil)

	body := `{"index":1,"term":2,"type":"STROKE","strokeId":"s1","userId":"u1","ts":123,"data":{"points":[{"x":1,"y":2}],"colour":"#fff","width":2,"tool":"pen"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/committed", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if len(mock.calls) != 0 {
		t.Fatalf("expected no broadcast calls, got %d", len(mock.calls))
	}
}

func TestInternalCommittedHandlerBroadcastsStrokeWithAuth(t *testing.T) {
	mock := &mockBroadcaster{}
	calledTs := int64(0)
	h := internalCommittedHandler(
		mock,
		zap.NewNop(),
		"good-token",
		func() string { return "replica2" },
		func() string { return "run-1" },
		func(ts int64) { calledTs = ts },
	)

	body := `{"index":1,"term":2,"type":"STROKE","strokeId":"s1","userId":"u1","ts":123,"data":{"points":[{"x":1,"y":2}],"colour":"#fff","width":2,"tool":"pen"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/committed", bytes.NewBufferString(body))
	req.Header.Set("X-Replica-Token", "good-token")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if calledTs != 123 {
		t.Fatalf("expected onCommitted to receive 123, got %d", calledTs)
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected one broadcast call, got %d", len(mock.calls))
	}
	if mock.calls[0].msgType != "STROKE_COMMITTED" {
		t.Fatalf("expected STROKE_COMMITTED, got %s", mock.calls[0].msgType)
	}

	payload, ok := mock.calls[0].payload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map payload")
	}
	if payload["strokeId"] != "s1" {
		t.Fatalf("expected strokeId s1, got %v", payload["strokeId"])
	}
	if payload["leaderId"] != "replica2" {
		t.Fatalf("expected leaderId replica2, got %v", payload["leaderId"])
	}
	if payload["recoveryRunId"] != "run-1" {
		t.Fatalf("expected recoveryRunId run-1, got %v", payload["recoveryRunId"])
	}
}
