package raft

import "testing"

func TestQuorumSize(t *testing.T) {
	tests := []struct {
		total int
		want  int
	}{
		{total: 0, want: 1},
		{total: 1, want: 1},
		{total: 2, want: 2},
		{total: 3, want: 2},
		{total: 4, want: 3},
		{total: 5, want: 3},
	}

	for _, tc := range tests {
		got := quorumSize(tc.total)
		if got != tc.want {
			t.Fatalf("quorumSize(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}
