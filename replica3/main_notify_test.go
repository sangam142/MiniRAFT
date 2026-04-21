package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	rafflog "miniraft/replica/log"

	"go.uber.org/zap"
)

func TestNotifyGatewayCommittedSetsReplicaToken(t *testing.T) {
	var (
		gotToken       string
		gotContentType string
		gotEntry       rafflog.LogEntry
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Replica-Token")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		_ = json.Unmarshal(body, &gotEntry)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	notifyGatewayCommitted(ts.URL, "replica-token", rafflog.LogEntry{
		Index:    3,
		Term:     9,
		StrokeID: "stroke-1",
		UserID:   "user-1",
	}, zap.NewNop())

	if gotToken != "replica-token" {
		t.Fatalf("expected token replica-token, got %q", gotToken)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected content-type application/json, got %q", gotContentType)
	}
	if gotEntry.Index != 3 || gotEntry.Term != 9 || gotEntry.StrokeID != "stroke-1" {
		t.Fatalf("unexpected entry payload: %+v", gotEntry)
	}
}
