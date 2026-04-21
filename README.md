# MiniRAFT — Distributed Raft Collaborative Canvas

MiniRAFT is a fault-tolerant collaborative whiteboard that uses the Raft consensus
algorithm to replicate every brushstroke across a four-node cluster in real time.
All drawing state is durable — the cluster survives complete restarts and single-node
failures without losing a stroke.

![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)
![gRPC](https://img.shields.io/badge/gRPC-protobuf-244c5a?logo=google&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)
![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black)
![License](https://img.shields.io/badge/license-coursework-lightgrey)

---

## Contents

- [Architecture](#architecture)
- [How it works](#how-it-works)
- [RAFT protocol parameters](#raft-protocol-parameters)
- [Quick start](#quick-start)
- [Demo scenarios](#demo-scenarios)
- [Failure scenarios](#failure-scenarios)
- [Team & component ownership](#team-component-ownership)
- [Recent additions](#recent-additions-v2)
- [Known limitations](#known-limitations)

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Browser Clients                          │
│               (Tab A / Tab B / Tab C)                        │
└────────────────────────┬────────────────┬────────────────────┘
                         │ WebSocket      │ SSE
                         │ (strokes)      │ (cluster status)
                         ▼               ▼
┌─────────────────────────────────────────────────────────────┐
│                    Gateway :8080                              │
│       Leader Tracker · WS Hub · Chaos API · SSE Hub          │
└──────────┬──────────────────┬──────────────────┬────────────┘
           │ HTTP POST        │ HTTP POST        │ HTTP POST
           │ /stroke          │ /stroke          │ /stroke
           │ HTTP GET         │ HTTP GET         │ HTTP GET
           │ /status          │ /status          │ /status
           ▼                  ▼                  ▼
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│  Replica 1   │  │  Replica 2   │  │  Replica 3   │
│  :9001 gRPC  │◄─┤  :9002 gRPC  ├─►│  :9003 gRPC  │
│  :8081 HTTP  │  │  :8082 HTTP  │  │  :8083 HTTP  │
│              │  │              │  │              │
│  ┌────────┐  │  │  ┌────────┐  │  │  ┌────────┐  │
│  │WAL log │  │  │  │WAL log │  │  │  │WAL log │  │
│  └────────┘  │  │  └────────┘  │  │  └────────┘  │
└──────────────┘  └──────────────┘  └──────────────┘
gRPC between replicas:
  RequestVote · AppendEntries · Heartbeat · SyncLog
```

**Protocol summary:**

| Path | Protocol | Purpose |
|------|----------|---------|
| Browser ↔ Gateway | WebSocket | Stroke drawing, undo, canvas sync |
| Browser ↔ Gateway | SSE | Cluster status push (leader, node health) |
| Browser → Gateway | HTTP REST | Chaos (stop/kill replica) |
| Gateway ↔ Replicas | HTTP REST | Forward strokes/undos, poll status, fetch log entries |
| Replica → Gateway | HTTP REST | Notify gateway on each log commit (`/internal/committed`) |
| Replica ↔ Replica | gRPC | Raft RPCs: RequestVote, AppendEntries, Heartbeat, SyncLog |

---

## How it works

### Consensus (RAFT)

Four replicas run mini-RAFT with randomised election timeouts (500–800 ms) to
prevent split votes, 150 ms heartbeats from the Leader, and a majority quorum of
3 of 4 nodes to commit any entry. All inter-replica communication uses gRPC with
Protocol Buffers. Term numbers enforce authority — any node receiving a message
with a higher term immediately steps down to Follower and updates its term before
processing the message, ensuring the cluster always converges on one Leader per
term.

### Log replication

Every drawing stroke (and every undo) is a log entry. The Leader appends the entry
to its local log, fans out `AppendEntries` RPCs to followers in parallel, and
marks the entry committed once a majority acknowledges it. A write-ahead log (WAL)
is flushed and synced to disk before the node responds to any RPC, so entries
survive container crashes and full cluster restarts. When a restarted node
reconnects it sends a single `SyncLog` RPC containing its current log length; the
Leader replies with the delta — one round-trip regardless of how far behind the
follower is.

### Gateway

The gateway is the single entry point for all browser clients. It polls all four
replica `/status` endpoints concurrently every 500 ms to track the current leader
and detect leadership changes. Incoming strokes are forwarded directly to the
leader's HTTP API; if no leader is known the gateway buffers the stroke in memory
for up to 2 seconds, then drains the buffer once a leader is elected. When a
replica commits a log entry it calls `POST /internal/committed` on the gateway,
which immediately broadcasts `STROKE_COMMITTED` or `UNDO_COMPENSATION` to every
connected WebSocket client. Chaos Mode talks to the Docker daemon directly over the
mounted Unix socket (`/var/run/docker.sock`) using a minimal stdlib HTTP client —
no external Docker SDK dependency.

### Frontend

The UI shell and dashboard are written in React; the drawing surface uses the
Vanilla JS Canvas 2D API directly (bypassing React's reconciler entirely for
paint-loop performance). Each browser client receives a UUID v4 user ID on
WebSocket connect, which is attached to every stroke for per-user colour identity.
Undo and redo flow through the full Raft pipeline: pressing undo sends a
`STROKE_UNDO` message to the gateway, which forwards it to the leader's `/undo`
endpoint; the leader appends an `UNDO_COMPENSATION` entry to the log, commits it
via Raft, and the gateway broadcasts the compensation to all clients — preserving
the append-only log invariant. The cluster status panel in the dashboard is fed by
an SSE stream and updates in real time showing each replica's state, term, log
length, and milliseconds since last heartbeat.

---

## Node state transitions

```
                         timeout / no heartbeat
                    ┌─────────────────────────────┐
                    │                             ▼
              ┌─────┴──────┐             ┌───────────────┐
  start ─────►│  FOLLOWER  │             │   CANDIDATE   │
              └─────┬──────┘             └───────┬───────┘
                    │                            │
                    │   higher term seen      split vote (no majority)
                    │◄──────────────────┐        │  → increment term, retry
                    │                   │        │
                    │             ┌─────┴──────┐ │ majority votes received
                    │             │   LEADER   │◄┘
                    └────────────►└────────────┘
                      higher term     │
                      seen in RPC     │ steps down on higher
                                      │ term in any RPC
                                      ▼
                                  FOLLOWER
```

**Transition rules:**

| From | To | Trigger |
|------|----|---------|
| Follower | Candidate | Election timer fires (no heartbeat in 500–800 ms) |
| Candidate | Leader | Receives ≥ 3 votes (majority of 4) in current term |
| Candidate | Follower | Sees higher term in any RPC response, or valid leader heartbeat arrives |
| Candidate | Candidate | Split vote — no majority reached; increment term and retry |
| Leader | Follower | Sees higher term in any incoming or outgoing RPC |

**Invariants:**
- Only one leader exists per term (at most)
- A node votes for at most one candidate per term
- A node only votes for a candidate whose log is at least as up-to-date as its own
- `currentTerm` is persisted to WAL before any RequestVote RPC is sent

---

## RAFT protocol parameters

| Parameter | Value | Why |
|-----------|-------|-----|
| Election timeout | 500–800 ms (random) | Randomisation prevents split votes |
| Heartbeat interval | 150 ms | Must be << election timeout |
| Vote RPC timeout | 300 ms | Allows slow nodes time to respond |
| Majority threshold | 3 of 4 nodes | Tolerates 1 simultaneous failure |
| Stroke buffer (no leader) | 2 seconds | Covers typical election duration |
| WAL sync | Every write | Guarantees durability on crash |
| SyncLog catch-up | Single RPC delta | Faster than per-heartbeat backfill |

---

## Quick start

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) and
  [Docker Compose v2](https://docs.docker.com/compose/install/)
- Go 1.22+ (for local development only — not needed to run with Docker)

### Run everything

```bash
git clone <repo-url> miniraft
cd miniraft
docker compose up --build
```

Open **http://localhost:3000** in your browser.

### Port reference

| Port | Service | Description |
|------|---------|-------------|
| `3000` | Frontend | React canvas + dashboard UI |
| `8080` | Gateway | WebSocket (`/ws`), SSE (`/events/cluster-status`), REST |
| `8081` | Replica 1 | HTTP status / stroke / entries / health (dev override only) |
| `8082` | Replica 2 | HTTP status / stroke / entries / health (dev override only) |
| `8083` | Replica 3 | HTTP status / stroke / entries / health (dev override only) |
| `8084` | Replica 4 | HTTP status / stroke / entries / health (dev override only) |
| `9001` | Replica 1 | gRPC (Raft RPCs) (dev override only) |
| `9002` | Replica 2 | gRPC (Raft RPCs) (dev override only) |
| `9003` | Replica 3 | gRPC (Raft RPCs) (dev override only) |
| `9004` | Replica 4 | gRPC (Raft RPCs) (dev override only) |

### Dev mode

```bash
# Uses Dockerfile.dev + air for hot-reload on all replicas
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

Edit any file in `replica/` and save — the affected container rebuilds
automatically, triggers a Raft election, and the system recovers without
dropping clients.

### Security environment notes

- `INTERNAL_COMMIT_TOKEN` is required by the gateway and replicas. The gateway
  rejects `/internal/committed` callbacks unless the request includes
  `X-Replica-Token` matching this value.
- Chaos is now strictly gated: if `CHAOS_ENABLED=true`, then `CHAOS_TOKEN` must
  be set or the gateway exits at startup.
- In dev mode, the frontend can send chaos auth automatically via
  `VITE_CHAOS_TOKEN` (configured in `docker-compose.dev.yml`).

### Run the smoke test

```bash
chmod +x scripts/smoke-test.sh
./scripts/smoke-test.sh
```

8 automated checks: boot, stroke commit, leader failover, post-failover stroke,
replica restart, catch-up verification. Requires `websocat`, `wscat`, or
Python `websockets`.

---

## Demo scenarios

### Kill the leader (chaos mode)

**Via UI:** click the **Chaos Mode** button (skull icon in toolbar), select a kill
mode, and click a replica node in the dashboard.

**Via CLI:**
```bash
# Stop a replica gracefully
docker stop miniraft-replica1-1

# Hard kill (simulates crash)
docker kill miniraft-replica1-1
```

A new leader is elected within 500–800 ms. Any strokes sent during the gap are
buffered at the gateway and committed once the new leader is elected (up to 2 s).

### View logs

```bash
docker logs -f miniraft-replica1-1
docker logs -f miniraft-gateway-1
```

### View cluster status

```bash
# All four replicas — returns state, term, logLength, leaderId
curl http://localhost:8081/status | jq .
curl http://localhost:8082/status | jq .
curl http://localhost:8083/status | jq .
curl http://localhost:8084/status | jq .
```

### View Prometheus metrics

```bash
curl http://localhost:8080/metrics
```

Individual replica metrics:
```bash
curl http://localhost:8081/metrics
```

### View committed log entries

```bash
curl http://localhost:8081/entries | jq .
```

### Automated smoke test

Requires `websocat` (or `wscat` / Python `websockets`) for WebSocket steps:

```bash
bash scripts/smoke-test.sh
```

The script brings up the cluster, sends a stroke, kills the leader, verifies
re-election within 3 s, sends a post-failover stroke, restarts the killed replica,
and confirms it catches up — then tears everything down.

---

## Failure scenarios

| Scenario | System behaviour |
|----------|-----------------|
| Leader dies | Election completes in < 800 ms. Gateway reroutes. No stroke loss if buffered. |
| Follower dies | System continues normally. Majority (3/4) still available. |
| Follower restarts | Replays WAL, requests SyncLog delta from leader, rejoins in one RPC. |
| 2 of 4 replicas die | System stalls (no majority). Strokes buffered at gateway. Recovers when quorum restored. |
| All 4 replicas restart | WAL replayed on each. Leader elected. CANVAS_SYNC restores all clients. |
| Leader dies mid-commit | Entry stays in some follower logs. New leader re-evaluates via log matching. Uncommitted entries may be lost — correct Raft behaviour. |
| Network partition (split brain) | Simulated via Chaos Mode "Partition" — disconnects a node from the Docker network without killing it. The isolated node runs elections it cannot win (no majority). When healed via "Heal Partition", it rejoins as follower, discovers the higher term, and catches up via SyncLog. |

---

## Team & component ownership

| Member | Component | Key files | Viva topics |
|--------|-----------|-----------|-------------|
| Shrish (PES2UG23AM097) | Raft Core Engine + Quorum | `replica/raft/node.go`, `election.go`, `heartbeat.go`, `rpc_server.go`, `quorum.go` | Election timer, split votes, term demotion, heartbeat loop cancellation, 4-node quorum logic |
| Saffiya (PES2UG23AM87) | Log Replication + WAL + Catch-up | `replica/log/log.go`, `wal.go`, `replication.go`, `wal_regression_test.go`, `replica/main.go` | prevLogIndex consistency, WAL durability, truncation safety, restart catch-up protocol |
| Rushad (PES2UG23AM061) | Gateway + Docker Chaos + Recovery Telemetry | `gateway/main.go`, `gateway/chaos/`, `gateway/anomaly/`, `gateway/recovery/`, `scripts/`, `docker-compose*.yml` | Leader routing, stroke buffering, Docker partition/heal, chaos auth, recovery run metrics/events |
| Ayesha (PES2UG23AM921) | Frontend + Dashboard + Undo/Redo + Provenance | `frontend/src/canvas/`, `frontend/src/dashboard/`, `frontend/src/chaos/`, `frontend/src/toolbar/`, `frontend/src/ws/` | Log-compensation undo, optimistic rendering, SSE dashboard, provenance inspection, reconnect sync |

Replica code is mirrored across `replica/`, `replica1/`, `replica2/`, and `replica3/` for dev hot-reload workflows.

---

## MiniRAFT protocol summary

- **Leader election** — each node starts an election timer (500–800 ms random). If
  no heartbeat arrives before it fires, the node becomes a Candidate, increments its
  term, and broadcasts `RequestVote` RPCs. The first Candidate to collect a majority
  (≥ 3/4) wins and becomes Leader.

- **Heartbeats** — the Leader sends `AppendEntries` (empty or with new entries) to
  all peers every 150 ms. If a follower misses enough heartbeats its timer fires and
  a new election begins. Heartbeats run in a dedicated goroutine that is cancelled
  immediately when the node steps down.

- **Log replication** — all mutations (stroke draws, undo compensations) are
  forwarded to the Leader's HTTP API. The Leader appends the entry to its log and
  replicates it to followers via `AppendEntries`. Once a majority acknowledges the
  entry it is committed and the Leader calls `POST /internal/committed` on the
  gateway, which broadcasts `STROKE_COMMITTED` / `UNDO_COMPENSATION` to all
  WebSocket clients.

- **Log catch-up** — a follower that falls behind (e.g. after restart) receives the
  missing entries in bulk via the `SyncLog` gRPC call, which the Leader initiates
  via `catchUpPeer` whenever an `AppendEntries` rejection signals a log gap.

- **Durability** — every log entry is written to a WAL (Write-Ahead Log) on disk
  and `fsync`-ed before the node acknowledges it. On restart, the WAL is replayed
  to restore the full log and commit index before the node joins the cluster.

- **Gateway buffering** — while the cluster has no leader (during elections) the
  gateway buffers incoming strokes in memory for up to 2 s. Once a leader is
  elected the buffer is drained and all pending strokes are committed. Strokes
  older than 2 s are dropped with an error returned to the originating client.

- **Optimistic UI** — strokes appear immediately on the drawing client at 70 %
  opacity (pending state) and switch to full opacity when `STROKE_COMMITTED` is
  received. On `CANVAS_SYNC` (reconnect / new tab) any locally pending strokes are
  re-sent so they are not lost during reconnects.

---

## Recent additions (v2)

- Expanded runtime topology from 3 to 4 replicas, with dynamic quorum (`3 of 4`).
- Added chaos `partition` and `heal` modes using Docker network connect/disconnect.
- Added secured gateway internals: `INTERNAL_COMMIT_TOKEN` and chaos token enforcement.
- Added gateway recovery tracker with SSE lifecycle events:
  `chaos_recovery_started`, `chaos_recovery_milestone`, `chaos_recovery_completed`.
- Added term anomaly detection and SSE alerts:
  `term_surge_detected`, `term_surge_resolved`.
- Added WS/SSE provenance metadata (`ts`, `leaderId`, `recoveryRunId`) for frontend inspection.
- Added regression/contract tests for quorum, WAL replay safety, gateway auth, anomaly detection, and recovery completion.
- Added hardened smoke/capture scripts for failover verification in mixed environments.

---

## Known limitations

- **Defensive truncation guard only:** `TruncateFrom()` refuses to truncate
  committed entries but correct Raft (Leader Completeness Property) guarantees
  this path is never reached in normal operation.

- **Pending stroke layer uses `globalAlpha`:** optimistic rendering draws pending
  strokes at 70 % opacity via `globalAlpha` on the full canvas context rather than
  a true separate compositing layer. Visually correct but architecturally impure.

- **No replica-level strokeId deduplication:** gateway deduplicates by `strokeId`
  (60-second window) but a duplicate that bypasses the gateway (e.g. direct
  `POST` to `/stroke`) would create two log entries with the same semantic stroke.
  Demo-safe — not reachable via the UI.

- **Four-node quorum requires 3 of 4:** with four replicas the majority threshold is
  3 (quorum of 4). This tolerates exactly one failure — same as a three-node cluster.
  To tolerate two simultaneous failures a five-node cluster is needed. `quorumSize()`
  already handles arbitrary cluster sizes correctly.
