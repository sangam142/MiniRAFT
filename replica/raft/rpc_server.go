package raft

import (
	"context"
	"encoding/json"
	"time"

	proto "miniraft/replica/proto"

	"go.uber.org/zap"
)

// RaftRPCServer implements proto.RaftServiceServer.
type RaftRPCServer struct {
	proto.UnimplementedRaftServiceServer
	node   *RaftNode
	logger *zap.Logger
}

// NewRaftRPCServer constructs a RaftRPCServer.
func NewRaftRPCServer(node *RaftNode, logger *zap.Logger) *RaftRPCServer {
	return &RaftRPCServer{node: node, logger: logger}
}

// RequestVote handles a vote request from a candidate.
func (s *RaftRPCServer) RequestVote(_ context.Context, req *proto.VoteRequest) (*proto.VoteResponse, error) {
	n := s.node
	n.mu.Lock()
	defer n.mu.Unlock()

	s.logger.Info("RequestVote received",
		zap.String("candidateId", req.CandidateId),
		zap.Int64("term", req.Term),
		zap.Int64("lastLogIndex", req.LastLogIndex),
		zap.Int64("lastLogTerm", req.LastLogTerm),
	)

	// Step down if we see a higher term.
	if req.Term > n.currentTerm {
		n.becomeFollowerLocked(req.Term, "")
	}

	deny := &proto.VoteResponse{Term: n.currentTerm, VoteGranted: false}

	// Candidate's term is stale.
	if req.Term < n.currentTerm {
		return deny, nil
	}

	// Already voted for someone else this term.
	if n.votedFor != "" && n.votedFor != req.CandidateId {
		return deny, nil
	}

	// Candidate log must be at least as up-to-date as ours (§5.4.1).
	myLastTerm := n.log.LastTerm()
	myLastIdx := n.log.LastIndex()
	candidateUpToDate := req.LastLogTerm > myLastTerm ||
		(req.LastLogTerm == myLastTerm && req.LastLogIndex >= myLastIdx)
	if !candidateUpToDate {
		return deny, nil
	}

	// Grant vote.
	n.votedFor = req.CandidateId
	if n.wal != nil {
		if err := n.wal.WriteVote(req.CandidateId); err != nil {
			s.logger.Warn("failed to persist granted vote",
				zap.String("candidateId", req.CandidateId),
				zap.Error(err),
			)
		} else if n.metrics != nil {
			n.metrics.IncrWALWrites()
		}
	}
	if n.metrics != nil {
		n.metrics.RaftVotesGranted.Inc()
	}

	s.logger.Info("vote granted",
		zap.String("candidateId", req.CandidateId),
		zap.Int64("term", req.Term),
	)

	// Reset election timer so we don't interrupt the new leader.
	n.requestElectionTimerReset()

	return &proto.VoteResponse{Term: n.currentTerm, VoteGranted: true}, nil
}

// AppendEntries handles an AppendEntries RPC from the leader.
func (s *RaftRPCServer) AppendEntries(_ context.Context, req *proto.AppendEntriesRequest) (*proto.AppendEntriesResponse, error) {
	n := s.node
	n.mu.Lock()

	s.logger.Debug("AppendEntries received",
		zap.String("leaderId", req.LeaderId),
		zap.Int64("term", req.Term),
		zap.Int("numEntries", len(req.Entries)),
		zap.Int64("prevLogIndex", req.PrevLogIndex),
	)

	// Stale leader — reject.
	if req.Term < n.currentTerm {
		currentTerm := n.currentTerm
		n.mu.Unlock()
		return &proto.AppendEntriesResponse{Term: currentTerm, Success: false}, nil
	}

	// Accept authority of the sender.
	if req.Term > n.currentTerm || n.state != Follower {
		n.becomeFollowerLocked(req.Term, req.LeaderId)
	} else {
		n.leaderID = req.LeaderId
	}
	n.lastHeartbeatMs = time.Now().UnixMilli()
	currentTerm := n.currentTerm
	n.mu.Unlock()

	// Reset timer outside the lock.
	n.requestElectionTimerReset()

	// Consistency check: verify prevLogIndex/prevLogTerm.
	if req.PrevLogIndex > 0 {
		prevEntry, ok := n.log.GetEntry(req.PrevLogIndex)
		if !ok {
			// We don't have the entry at prevLogIndex.
			lastIdx := n.log.LastIndex()
			s.logger.Debug("AppendEntries rejected: missing prevLogIndex",
				zap.Int64("prevLogIndex", req.PrevLogIndex),
				zap.Int64("lastIdx", lastIdx),
			)
			return &proto.AppendEntriesResponse{
				Term:          currentTerm,
				Success:       false,
				ConflictIndex: lastIdx + 1,
			}, nil
		}
		if prevEntry.Term != req.PrevLogTerm {
			s.logger.Debug("AppendEntries rejected: prevLogTerm mismatch",
				zap.Int64("prevLogIndex", req.PrevLogIndex),
				zap.Int64("expected", req.PrevLogTerm),
				zap.Int64("got", prevEntry.Term),
			)
			return &proto.AppendEntriesResponse{
				Term:          currentTerm,
				Success:       false,
				ConflictIndex: req.PrevLogIndex,
			}, nil
		}
	}

	// Append entries, truncating on conflict.
	for _, pe := range req.Entries {
		existing, ok := n.log.GetEntry(pe.Index)
		if ok {
			if existing.Term != pe.Term {
				// Conflict: truncate our log from this index onward.
				if err := n.log.TruncateFrom(pe.Index); err != nil {
					s.logger.Error("TruncateFrom rejected — refusing to truncate committed entry",
						zap.Error(err))
					return &proto.AppendEntriesResponse{
						Term:    currentTerm,
						Success: false,
					}, nil
				}
				if err := n.log.AppendEntry(protoToLogEntry(pe)); err != nil {
					s.logger.Error("AppendEntry failed", zap.Error(err))
				}
			}
			// else: already have a matching entry — skip.
		} else {
			if err := n.log.AppendEntry(protoToLogEntry(pe)); err != nil {
				s.logger.Error("AppendEntry failed", zap.Error(err))
			}
		}
	}

	// Advance commit index.
	if req.LeaderCommit > n.log.GetCommitIndex() {
		newCommit := req.LeaderCommit
		if lastIdx := n.log.LastIndex(); lastIdx < newCommit {
			newCommit = lastIdx
		}
		if newCommit > n.log.GetCommitIndex() {
			n.log.Commit(newCommit) //nolint:errcheck
			n.mu.Lock()
			n.commitIndex = n.log.GetCommitIndex()
			n.mu.Unlock()
		}
	}

	return &proto.AppendEntriesResponse{Term: currentTerm, Success: true}, nil
}

// Heartbeat handles a heartbeat from the leader.
func (s *RaftRPCServer) Heartbeat(_ context.Context, req *proto.HeartbeatRequest) (*proto.HeartbeatResponse, error) {
	n := s.node
	n.mu.Lock()

	s.logger.Debug("Heartbeat received",
		zap.String("leaderId", req.LeaderId),
		zap.Int64("term", req.Term),
	)

	if req.Term < n.currentTerm {
		currentTerm := n.currentTerm
		n.mu.Unlock()
		return &proto.HeartbeatResponse{Term: currentTerm, Success: false}, nil
	}

	if req.Term > n.currentTerm || n.state != Follower {
		n.becomeFollowerLocked(req.Term, req.LeaderId)
	} else {
		n.leaderID = req.LeaderId
	}
	n.lastHeartbeatMs = time.Now().UnixMilli()
	currentTerm := n.currentTerm

	// Advance commit index from heartbeat.
	if req.CommitIndex > n.commitIndex {
		newCommit := req.CommitIndex
		if lastIdx := n.log.LastIndex(); lastIdx < newCommit {
			newCommit = lastIdx
		}
		if newCommit > n.commitIndex {
			n.log.Commit(newCommit) //nolint:errcheck
			n.commitIndex = n.log.GetCommitIndex()
		}
	}

	n.mu.Unlock()
	n.requestElectionTimerReset()

	return &proto.HeartbeatResponse{Term: currentTerm, Success: true}, nil
}

// SyncLog returns log entries from fromIndex onward (for catch-up).
func (s *RaftRPCServer) SyncLog(_ context.Context, req *proto.SyncLogRequest) (*proto.SyncLogResponse, error) {
	n := s.node

	s.logger.Debug("SyncLog received",
		zap.String("replicaId", req.ReplicaId),
		zap.Int64("fromIndex", req.FromIndex),
		zap.Int64("term", req.Term),
	)

	// SyncLog is read-only and safe for startup catch-up. We do not reject
	// stale caller terms here because restarted followers often start at term 0.
	n.mu.Lock()
	currentTerm := n.currentTerm
	n.mu.Unlock()

	if req.Term > currentTerm {
		n.BecomeFollower(req.Term, "")
	}

	entries := n.log.GetEntriesFrom(req.FromIndex)
	commitIndex := n.log.GetCommitIndex()

	protoEntries := make([]*proto.LogEntry, len(entries))
	for i, e := range entries {
		data, _ := json.Marshal(e.Data)
		protoEntries[i] = &proto.LogEntry{
			Index:     e.Index,
			Term:      e.Term,
			Type:      string(e.Type),
			StrokeId:  e.StrokeID,
			UserId:    e.UserID,
			Data:      data,
			Timestamp: e.Timestamp,
		}
	}

	return &proto.SyncLogResponse{
		Entries:     protoEntries,
		CommitIndex: commitIndex,
	}, nil
}

