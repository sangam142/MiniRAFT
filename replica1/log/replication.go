package log

import (
	"go.uber.org/zap"
)

// AppendEntriesHandler validates and applies AppendEntries RPCs to the log.
type AppendEntriesHandler struct {
	log    *RaftLog
	logger *zap.Logger
}

func NewAppendEntriesHandler(log *RaftLog, logger *zap.Logger) *AppendEntriesHandler {
	return &AppendEntriesHandler{log: log, logger: logger}
}

// HandleAppendEntries applies the consistency check and appends entries to the log.
// Returns (success, conflictIndex):
//   - success=false + conflictIndex set means the leader should retry from conflictIndex.
//   - success=true means all entries were applied and commitIndex was advanced.
func (h *AppendEntriesHandler) HandleAppendEntries(
	prevLogIndex int64,
	prevLogTerm int64,
	entries []LogEntry,
	leaderCommit int64,
) (success bool, conflictIndex int64) {
	if prevLogIndex > 0 {
		prev, ok := h.log.GetEntry(prevLogIndex)
		if !ok {
			h.logger.Debug("HandleAppendEntries: missing prevLogIndex",
				zap.Int64("prevLogIndex", prevLogIndex),
			)
			return false, h.log.LastIndex() + 1
		}
		if prev.Term != prevLogTerm {
			h.logger.Debug("HandleAppendEntries: prevLogTerm mismatch",
				zap.Int64("prevLogIndex", prevLogIndex),
				zap.Int64("expected", prevLogTerm),
				zap.Int64("got", prev.Term),
			)
			return false, prevLogIndex
		}
	}

	for _, entry := range entries {
		existing, ok := h.log.GetEntry(entry.Index)
		if ok {
			if existing.Term != entry.Term {
				h.log.TruncateFrom(entry.Index)
				h.log.AppendEntry(entry) //nolint:errcheck
			}
		} else {
			h.log.AppendEntry(entry) //nolint:errcheck
		}
	}

	if leaderCommit > h.log.GetCommitIndex() {
		newCommit := leaderCommit
		if lastIdx := h.log.LastIndex(); lastIdx < newCommit {
			newCommit = lastIdx
		}
		h.log.Commit(newCommit) //nolint:errcheck
	}

	return true, 0
}
