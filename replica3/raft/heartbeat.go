package raft

import (
	"context"
	"encoding/json"
	"time"

	rafflog "miniraft/replica/log"
	proto "miniraft/replica/proto"

	"go.uber.org/zap"
)

const heartbeatInterval = 150 * time.Millisecond

// sendHeartbeats runs in its own goroutine while the node is Leader.
// Every heartbeatInterval it sends AppendEntries to every peer. When there are
// no new entries, the RPC is an empty AppendEntries heartbeat that still carries
// prevLogIndex/prevLogTerm for consistency checks.
// It exits when ctx is cancelled (via BecomeFollower or Stop).
func (n *RaftNode) sendHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.mu.Lock()
			if n.state != Leader {
				n.mu.Unlock()
				return
			}

			term := n.currentTerm
			leaderID := n.id
			commitIndex := n.commitIndex
			lastIdx := n.log.LastIndex()

			type peerTask struct {
				peer         string
				prevLogIndex int64
				prevLogTerm  int64
				entries      []rafflog.LogEntry
				nextIdx      int64
			}

			tasks := make([]peerTask, 0, len(n.peers))
			for _, peer := range n.peers {
				ni := n.nextIndex[peer]
				if ni <= 0 {
					ni = 1
				}

				prevLogIndex := ni - 1
				var prevLogTerm int64
				if prevLogIndex > 0 {
					if e, ok := n.log.GetEntry(prevLogIndex); ok {
						prevLogTerm = e.Term
					}
				}

				var entries []rafflog.LogEntry
				if ni <= lastIdx {
					entries = n.log.GetEntriesFrom(ni)
				}

				tasks = append(tasks, peerTask{
					peer:         peer,
					prevLogIndex: prevLogIndex,
					prevLogTerm:  prevLogTerm,
					entries:      entries,
					nextIdx:      ni,
				})
			}
			n.mu.Unlock()

			for _, task := range tasks {
				go func(task peerTask) {
					client, ok := n.getPeerClient(task.peer)
					if !ok {
						return
					}

					protoEntries := make([]*proto.LogEntry, len(task.entries))
					for i, e := range task.entries {
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

					timeout := 350 * time.Millisecond
					if len(task.entries) > 0 {
						timeout = 500 * time.Millisecond
					}

					start := time.Now()
					rpcCtx, rpcCancel := context.WithTimeout(ctx, timeout)
					resp, err := client.AppendEntries(rpcCtx, &proto.AppendEntriesRequest{
						Term:         term,
						LeaderId:     leaderID,
						PrevLogIndex: task.prevLogIndex,
						PrevLogTerm:  task.prevLogTerm,
						Entries:      protoEntries,
						LeaderCommit: commitIndex,
					})
					rpcCancel()

					if n.metrics != nil {
						n.metrics.RaftHeartbeatLatency.Observe(time.Since(start).Seconds())
					}

					if err != nil {
						if ctx.Err() != nil {
							return // stepdown cancelled us, not an error
						}
						n.logger.Debug("AppendEntries RPC failed",
							zap.String("peer", task.peer),
							zap.Error(err),
						)
						return
					}

					if resp.Term > term {
						n.BecomeFollower(resp.Term, "")
						return
					}

					if resp.Success {
						if len(task.entries) > 0 {
							n.mu.Lock()
							lastNewIdx := task.nextIdx + int64(len(task.entries)) - 1
							if lastNewIdx > n.matchIndex[task.peer] {
								n.matchIndex[task.peer] = lastNewIdx
								n.nextIndex[task.peer] = lastNewIdx + 1
							}
							n.tryAdvanceCommitIndex()
							n.mu.Unlock()
						}
					} else {
						// Back off nextIndex using the follower's conflict hint.
						n.mu.Lock()
						if resp.ConflictIndex > 0 {
							n.nextIndex[task.peer] = resp.ConflictIndex
						} else if n.nextIndex[task.peer] > 1 {
							n.nextIndex[task.peer]--
						}
						conflictIdx := n.nextIndex[task.peer]
						n.mu.Unlock()

						// Immediately push missing entries without waiting for the next tick.
						go n.catchUpPeer(ctx, task.peer, conflictIdx)
					}

					if n.metrics != nil {
						n.metrics.RaftHeartbeatsSent.Inc()
					}
				}(task)
			}

		case <-ctx.Done():
			n.logger.Debug("heartbeat sender stopped (context cancelled)")
			return
		}
	}
}

// catchUpPeer immediately sends all log entries from fromIndex to a specific peer via
// AppendEntries — called when a heartbeat reveals a log mismatch, so we don't wait
// 150ms for the next tick to retry. ctx is the heartbeat context; cancellation on
// stepdown exits the in-flight RPC immediately.
func (n *RaftNode) catchUpPeer(ctx context.Context, peer string, fromIndex int64) {
	client, ok := n.getPeerClient(peer)
	if !ok {
		return
	}

	entries := n.log.GetEntriesFrom(fromIndex)
	if len(entries) == 0 {
		return
	}

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

	n.mu.Lock()
	if n.state != Leader {
		n.mu.Unlock()
		return
	}
	currentTerm := n.currentTerm
	leaderID := n.id
	commitIndex := n.commitIndex
	var prevLogIndex, prevLogTerm int64
	if fromIndex > 1 {
		if prev, ok2 := n.log.GetEntry(fromIndex - 1); ok2 {
			prevLogIndex = prev.Index
			prevLogTerm = prev.Term
		}
	}
	n.mu.Unlock()

	rpcCtx, rpcCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rpcCancel()

	resp, err := client.AppendEntries(rpcCtx, &proto.AppendEntriesRequest{
		Term:         currentTerm,
		LeaderId:     leaderID,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      protoEntries,
		LeaderCommit: commitIndex,
	})
	if err != nil {
		if ctx.Err() != nil {
			return // stepdown cancelled us
		}
		n.logger.Debug("catchUpPeer AppendEntries failed", zap.String("peer", peer), zap.Error(err))
		return
	}
	if resp.Term > currentTerm {
		n.BecomeFollower(resp.Term, "")
		return
	}
	if resp.Success {
		n.mu.Lock()
		lastNewIdx := fromIndex + int64(len(entries)) - 1
		if lastNewIdx > n.matchIndex[peer] {
			n.matchIndex[peer] = lastNewIdx
			n.nextIndex[peer] = lastNewIdx + 1
		}
		n.tryAdvanceCommitIndex()
		n.mu.Unlock()
	}
}
