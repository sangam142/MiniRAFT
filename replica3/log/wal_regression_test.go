package log

import (
	"testing"

	"go.uber.org/zap"
)

// TestReplayConflictRewrites verifies replay resolves historical conflicting
// entries by keeping the latest version for a given index.
func TestReplayConflictRewrites(t *testing.T) {
	tmp := t.TempDir()
	logger := zap.NewNop()

	wal, err := NewWAL(tmp, logger)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	t.Cleanup(func() {
		_ = wal.Close()
	})

	// Original suffix written first.
	if err := wal.WriteEntry(LogEntry{Index: 1, Term: 1, Type: EntryTypeStroke, StrokeID: "s1"}); err != nil {
		t.Fatalf("WriteEntry(1,1) failed: %v", err)
	}
	if err := wal.WriteEntry(LogEntry{Index: 2, Term: 1, Type: EntryTypeStroke, StrokeID: "s2-old"}); err != nil {
		t.Fatalf("WriteEntry(2,1) failed: %v", err)
	}
	if err := wal.WriteEntry(LogEntry{Index: 3, Term: 1, Type: EntryTypeStroke, StrokeID: "s3-old"}); err != nil {
		t.Fatalf("WriteEntry(3,1) failed: %v", err)
	}

	// Conflict rewrite (as would happen after follower truncation + append).
	if err := wal.WriteEntry(LogEntry{Index: 2, Term: 2, Type: EntryTypeStroke, StrokeID: "s2-new"}); err != nil {
		t.Fatalf("WriteEntry(2,2) failed: %v", err)
	}
	if err := wal.WriteEntry(LogEntry{Index: 3, Term: 2, Type: EntryTypeStroke, StrokeID: "s3-new"}); err != nil {
		t.Fatalf("WriteEntry(3,2) failed: %v", err)
	}

	state, err := wal.Replay()
	if err != nil {
		t.Fatalf("Replay failed: %v", err)
	}

	if got, want := len(state.Entries), 3; got != want {
		t.Fatalf("unexpected entry count after replay: got=%d want=%d", got, want)
	}

	if got := state.Entries[1].Term; got != 2 {
		t.Fatalf("index 2 term mismatch: got=%d want=2", got)
	}
	if got := state.Entries[1].StrokeID; got != "s2-new" {
		t.Fatalf("index 2 stroke mismatch: got=%q want=%q", got, "s2-new")
	}

	if got := state.Entries[2].Term; got != 2 {
		t.Fatalf("index 3 term mismatch: got=%d want=2", got)
	}
	if got := state.Entries[2].StrokeID; got != "s3-new" {
		t.Fatalf("index 3 stroke mismatch: got=%q want=%q", got, "s3-new")
	}
}

// TestGetEntryReturnsLatestDuplicate protects old WAL files where duplicate
// index records may still exist.
func TestGetEntryReturnsLatestDuplicate(t *testing.T) {
	rl := NewRaftLog(nil, zap.NewNop())

	if err := rl.AppendEntry(LogEntry{Index: 7, Term: 1, StrokeID: "old"}); err != nil {
		t.Fatalf("append old failed: %v", err)
	}
	if err := rl.AppendEntry(LogEntry{Index: 7, Term: 4, StrokeID: "new"}); err != nil {
		t.Fatalf("append new failed: %v", err)
	}

	e, ok := rl.GetEntry(7)
	if !ok {
		t.Fatalf("expected entry index 7")
	}
	if got, want := e.Term, int64(4); got != want {
		t.Fatalf("unexpected term: got=%d want=%d", got, want)
	}
	if got, want := e.StrokeID, "new"; got != want {
		t.Fatalf("unexpected stroke id: got=%q want=%q", got, want)
	}
}

func TestReplayCommitIndexCappedToLastEntry(t *testing.T) {
	tmp := t.TempDir()
	logger := zap.NewNop()

	wal, err := NewWAL(tmp, logger)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	t.Cleanup(func() {
		_ = wal.Close()
	})

	if err := wal.WriteEntry(LogEntry{Index: 1, Term: 1, Type: EntryTypeStroke, StrokeID: "s1"}); err != nil {
		t.Fatalf("WriteEntry(1) failed: %v", err)
	}
	if err := wal.WriteEntry(LogEntry{Index: 2, Term: 1, Type: EntryTypeStroke, StrokeID: "s2"}); err != nil {
		t.Fatalf("WriteEntry(2) failed: %v", err)
	}
	if err := wal.WriteCommit(5); err != nil {
		t.Fatalf("WriteCommit(5) failed: %v", err)
	}

	state, err := wal.Replay()
	if err != nil {
		t.Fatalf("Replay failed: %v", err)
	}

	if got, want := len(state.Entries), 2; got != want {
		t.Fatalf("unexpected entry count: got=%d want=%d", got, want)
	}
	if got, want := state.CommitIndex, int64(2); got != want {
		t.Fatalf("commitIndex should be capped to last entry: got=%d want=%d", got, want)
	}
}
