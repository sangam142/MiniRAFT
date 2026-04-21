package raft

import (
	"context"
	"math/rand"
	"time"

	proto "miniraft/replica/proto"

	"go.uber.org/zap"
)

// randomTimeout returns a random election timeout between 500 and 800 ms.
func randomTimeout() time.Duration {
	return time.Duration(500+rand.Intn(300)) * time.Millisecond
}

// requestElectionTimerReset queues an election timer reset request.
// The actual Stop/Reset operations are performed only by the election loop goroutine.
func (n *RaftNode) requestElectionTimerReset() {
	select {
	case n.electionResetCh <- struct{}{}:
	default:
		// A pending reset is already queued.
	}
}

// rearmElectionTimer stops the current election timer and re-arms it with a new random
// timeout. Must only be called by the election loop goroutine.
func (n *RaftNode) rearmElectionTimer() {
	if n.electionTimer == nil {
		return
	}

	if !n.electionTimer.Stop() {
		// Drain the channel if the timer already fired but hasn't been read yet.
		select {
		case <-n.electionTimer.C:
		default:
		}
	}
	n.electionTimer.Reset(randomTimeout())
}

// startElection runs a full RAFT election: sends RequestVote to all peers and
// transitions to leader if a majority vote is obtained.
func (n *RaftNode) startElection() {
	n.mu.Lock()

	// Only followers and candidates start elections.
	if n.state == Leader {
		n.mu.Unlock()
		return
	}

	n.state = Candidate
	n.currentTerm++
	term := n.currentTerm
	n.votedFor = n.id

	// Persist term and vote to WAL.
	if n.wal != nil {
		if err := n.wal.WriteTerm(term); err != nil {
			n.logger.Warn("failed to persist term", zap.Int64("term", term), zap.Error(err))
		} else if n.metrics != nil {
			n.metrics.IncrWALWrites()
		}
		if err := n.wal.WriteVote(n.id); err != nil {
			n.logger.Warn("failed to persist vote", zap.String("votedFor", n.id), zap.Error(err))
		} else if n.metrics != nil {
			n.metrics.IncrWALWrites()
		}
	}

	if n.metrics != nil {
		n.metrics.RaftElectionsTotal.Inc()
		n.metrics.RaftTerm.Set(float64(term))
		n.metrics.RaftState.Set(float64(Candidate))
	}

	lastLogIndex := n.log.LastIndex()
	lastLogTerm := n.log.LastTerm()
	peers := make([]string, len(n.peers))
	copy(peers, n.peers)

	n.mu.Unlock()

	n.logger.Info("starting election", zap.Int64("term", term), zap.String("id", n.id))
	n.rearmElectionTimer()

	if len(peers) == 0 {
		// Single-node cluster: win immediately.
		n.BecomeLeader()
		return
	}

	voteCh := make(chan bool, len(peers))

	for _, peer := range peers {
		go func(peer string) {
			client, ok := n.getPeerClient(peer)
			if !ok {
				voteCh <- false
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			resp, err := client.RequestVote(ctx, &proto.VoteRequest{
				Term:         term,
				CandidateId:  n.id,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  lastLogTerm,
			})
			if err != nil {
				n.logger.Warn("RequestVote RPC failed",
					zap.String("peer", peer),
					zap.Int64("term", term),
					zap.Error(err),
				)
				voteCh <- false
				return
			}

			// If a higher term is seen, step down immediately.
			if resp.Term > term {
				n.BecomeFollower(resp.Term, "")
				voteCh <- false
				return
			}

			voteCh <- resp.VoteGranted
		}(peer)
	}

	// Collect responses.
	votes := 1 // vote for self
	totalNodes := len(peers) + 1
	needed := quorumSize(totalNodes)
	responded := 0

	for responded < len(peers) {
		granted := <-voteCh
		responded++
		if granted {
			votes++
		}
		if votes >= needed {
			break
		}
		// Early exit: even if all remaining respond positively, we can't win.
		remaining := len(peers) - responded
		if votes+remaining < needed {
			break
		}
	}

	n.mu.Lock()
	// Only become leader if still candidate in the same term.
	if n.state != Candidate || n.currentTerm != term {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	if votes >= needed {
		n.logger.Info("election won", zap.Int64("term", term), zap.Int("votes", votes))
		n.BecomeLeader()
	} else {
		n.logger.Info("election lost", zap.Int64("term", term), zap.Int("votes", votes))
	}
}
