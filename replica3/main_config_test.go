package main

import "testing"

func TestParsePeerAddrs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{name: "empty", raw: "", wantLen: 0, wantErr: false},
		{name: "valid single", raw: "replica2:9001", wantLen: 1, wantErr: false},
		{name: "valid multiple", raw: "replica2:9001,replica3:9001", wantLen: 2, wantErr: false},
		{name: "invalid format", raw: "replica2", wantLen: 0, wantErr: true},
		{name: "invalid port", raw: "replica2:notaport", wantLen: 0, wantErr: true},
		{name: "out of range port", raw: "replica2:70000", wantLen: 0, wantErr: true},
		{name: "duplicate", raw: "replica2:9001,replica2:9001", wantLen: 0, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			peers, err := parsePeerAddrs(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			if len(peers) != tc.wantLen {
				t.Fatalf("unexpected peers length: got=%d want=%d", len(peers), tc.wantLen)
			}
		})
	}
}
