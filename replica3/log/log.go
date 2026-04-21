package log

import (
	"fmt"
	"sync"

	"go.uber.org/zap"
)

type RaftLog struct {
	mu          sync.RWMutex
	entries     []LogEntry
	commitIndex int64
	wal         *WAL
	walMetrics  interface{ IncrWALWrites() }
	logger      *zap.Logger
}

func NewRaftLog(wal *WAL, logger *zap.Logger, walMetrics ...interface{ IncrWALWrites() }) *RaftLog {
	var wm interface{ IncrWALWrites() }
	if len(walMetrics) > 0 {
		wm = walMetrics[0]
	}

	return &RaftLog{
		entries:    []LogEntry{},
		wal:        wal,
		walMetrics: wm,
		logger:     logger,
	}
}

func (l *RaftLog) AppendEntry(entry LogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.wal != nil {
		if err := l.wal.WriteEntry(entry); err != nil {
			return fmt.Errorf("WAL write failed: %w", err)
		}
		if l.walMetrics != nil {
			l.walMetrics.IncrWALWrites()
		}
	}
	l.entries = append(l.entries, entry)
	l.logger.Debug("appended log entry", zap.Int64("index", entry.Index), zap.Int64("term", entry.Term))
	return nil
}

func (l *RaftLog) GetEntry(index int64) (LogEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.Index == index {
			return e, true
		}
	}
	return LogEntry{}, false
}

func (l *RaftLog) GetEntriesFrom(fromIndex int64) []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var result []LogEntry
	for _, e := range l.entries {
		if e.Index >= fromIndex {
			result = append(result, e)
		}
	}
	return result
}

func (l *RaftLog) Commit(index int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.wal != nil {
		if err := l.wal.WriteCommit(index); err != nil {
			return fmt.Errorf("WAL commit write failed: %w", err)
		}
		if l.walMetrics != nil {
			l.walMetrics.IncrWALWrites()
		}
	}
	l.commitIndex = index
	l.logger.Debug("committed log up to index", zap.Int64("commitIndex", index))
	return nil
}

func (l *RaftLog) LastIndex() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if len(l.entries) == 0 {
		return 0
	}
	return l.entries[len(l.entries)-1].Index
}

func (l *RaftLog) LastTerm() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if len(l.entries) == 0 {
		return 0
	}
	return l.entries[len(l.entries)-1].Term
}

func (l *RaftLog) LoadFromWAL() error {
	if l.wal == nil {
		return nil
	}
	state, err := l.wal.Replay()
	if err != nil {
		return fmt.Errorf("WAL replay failed: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = state.Entries
	l.commitIndex = state.CommitIndex
	l.logger.Info("loaded log from WAL",
		zap.Int("entries", len(l.entries)),
		zap.Int64("commitIndex", l.commitIndex),
	)
	return nil
}

// TruncateFrom removes all entries with Index >= fromIndex from memory and
// persists the truncate intent to the WAL so replay preserves the same suffix.
// Returns an error if fromIndex is at or below the commit index — truncating
// committed entries would violate the Raft Leader Completeness property and
// indicates a bug in the caller.
func (l *RaftLog) TruncateFrom(fromIndex int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if fromIndex <= l.commitIndex {
		return fmt.Errorf(
			"refusing to truncate committed entry: index %d <= commitIndex %d",
			fromIndex, l.commitIndex,
		)
	}

	if l.wal != nil {
		if err := l.wal.WriteTruncate(fromIndex); err != nil {
			return fmt.Errorf("WAL truncate write failed: %w", err)
		}
		if l.walMetrics != nil {
			l.walMetrics.IncrWALWrites()
		}
	}

	cut := len(l.entries)
	for i, e := range l.entries {
		if e.Index >= fromIndex {
			cut = i
			break
		}
	}
	l.entries = l.entries[:cut]
	l.logger.Debug("truncated log", zap.Int64("fromIndex", fromIndex), zap.Int("remaining", len(l.entries)))
	return nil
}

func (l *RaftLog) AllEntries() []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	result := make([]LogEntry, len(l.entries))
	copy(result, l.entries)
	return result
}

func (l *RaftLog) GetCommitIndex() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.commitIndex
}
