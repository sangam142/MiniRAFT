#!/usr/bin/env bash
# capture-logs.sh — Capture a live failover demonstration and save logs.
#
# Usage:
#   bash scripts/capture-logs.sh
#
# What it does:
#   1. Waits for the cluster to be healthy (up to 60 s)
#   2. Identifies the current leader
#   3. Starts background log capture for all services
#   4. Kills the leader container (hard kill)
#   5. Waits for a new leader to be elected (up to 10 s)
#   6. Stops log capture and writes to logs/gateway/
#
# Prerequisites:
#   docker compose up --build -d   (cluster must already be running)

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
INFO() { printf "${YELLOW}[INFO]${NC} %s\n" "$1"; }
PASS() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
FAIL() { printf "${RED}[FAIL]${NC} %s\n" "$1"; }

if command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  COMPOSE="docker compose"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
LOG_DIR="$REPO_ROOT/logs/gateway"
REPLICA_IDS=(1 2 3 4)
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUTFILE="$LOG_DIR/failover_${TIMESTAMP}.log"

mkdir -p "$LOG_DIR"

cd "$REPO_ROOT"

# ── 1. Wait for cluster to be healthy ────────────────────────────────────────
INFO "Waiting for cluster to be healthy (up to 60 s)..."
deadline=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  healthy=0
  for i in "${REPLICA_IDS[@]}"; do
    if $COMPOSE exec -T "replica${i}" sh -lc "curl -sf http://localhost:8081/health" >/dev/null 2>&1; then
      healthy=$(( healthy + 1 ))
    fi
  done
  if [ "$healthy" -eq "${#REPLICA_IDS[@]}" ]; then
    PASS "All ${#REPLICA_IDS[@]} replicas healthy"
    break
  fi
  sleep 2
done

# ── 2. Identify leader ────────────────────────────────────────────────────────
INFO "Finding current leader..."
LEADER_IDX=""
for i in "${REPLICA_IDS[@]}"; do
  status=$($COMPOSE exec -T "replica${i}" sh -lc "curl -sf http://localhost:8081/status" 2>/dev/null || true)
  state=$(echo "$status" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('state',''))" 2>/dev/null || true)
  if [ "$state" = "LEADER" ]; then
    LEADER_IDX="$i"
    INFO "Leader is replica${i}"
    break
  fi
done

if [ -z "$LEADER_IDX" ]; then
  FAIL "No leader found — is the cluster running?"
  exit 1
fi

# ── 3. Start background log capture ──────────────────────────────────────────
INFO "Starting log capture → $OUTFILE"
{
  echo "=== MiniRAFT Failover Log — $TIMESTAMP ==="
  echo "=== Leader before failover: replica${LEADER_IDX} ==="
  echo ""
  $COMPOSE logs --no-color --tail=20 2>&1
} >> "$OUTFILE"

# Stream live logs in background, capture PID to stop later
$COMPOSE logs --no-color -f 2>&1 | sed "s/^/[LIVE] /" >> "$OUTFILE" &
LOG_PID=$!
trap "kill $LOG_PID 2>/dev/null || true" EXIT

# ── 4. Kill the leader ────────────────────────────────────────────────────────
project=$(cd "$REPO_ROOT" && $COMPOSE config --format json 2>/dev/null | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('name','miniraft'))" 2>/dev/null || basename "$REPO_ROOT" | tr '[:upper:]' '[:lower:]')
CONTAINER="${project}-replica${LEADER_IDX}-1"
INFO "Hard-killing $CONTAINER..."
{
  echo ""
  echo "=== CHAOS: hard kill $CONTAINER at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
} >> "$OUTFILE"

docker kill "$CONTAINER" 2>&1 | tee -a "$OUTFILE" || true

# ── 5. Wait for new leader ────────────────────────────────────────────────────
INFO "Waiting for new leader (up to 10 s)..."
NEW_LEADER=""
deadline=$(( $(date +%s) + 10 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  for i in "${REPLICA_IDS[@]}"; do
    [ "$i" = "$LEADER_IDX" ] && continue
    status=$($COMPOSE exec -T "replica${i}" sh -lc "curl -sf http://localhost:8081/status" 2>/dev/null || true)
    state=$(echo "$status" | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('state',''))" 2>/dev/null || true)
    if [ "$state" = "LEADER" ]; then
      NEW_LEADER="replica${i}"
      break 2
    fi
  done
  sleep 0.5
done

if [ -n "$NEW_LEADER" ]; then
  PASS "New leader elected: $NEW_LEADER"
  echo "=== NEW LEADER: $NEW_LEADER ===" >> "$OUTFILE"
else
  FAIL "No new leader within 10 s"
  echo "=== WARN: no new leader detected ===" >> "$OUTFILE"
fi

# ── 6. Collect final state ────────────────────────────────────────────────────
sleep 1
{
  echo ""
  echo "=== Final cluster status at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
  for i in "${REPLICA_IDS[@]}"; do
    echo "--- replica${i} ---"
    $COMPOSE exec -T "replica${i}" sh -lc "curl -sf http://localhost:8081/status" 2>/dev/null || echo "(unreachable)"
  done
} >> "$OUTFILE"

kill "$LOG_PID" 2>/dev/null || true
trap - EXIT

PASS "Failover log saved: $OUTFILE"
