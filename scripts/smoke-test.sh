#!/usr/bin/env bash
# smoke-test.sh - MiniRAFT end-to-end smoke test
# Requires: docker, docker compose (v2), curl
# Optional helpers: jq, websocat, wscat, python3+websockets
# Usage: bash scripts/smoke-test.sh
# Exit 0 only if every step passes.

set -euo pipefail

# --- Color helpers -----------------------------------------------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
FAIL() { printf "${RED}[FAIL]${NC} %s\n" "$1"; FAILURES=$((FAILURES+1)); }
SKIP() { printf "${YELLOW}[SKIP]${NC} %s\n" "$1"; SKIPS=$((SKIPS+1)); }
INFO() { printf "${YELLOW}[INFO]${NC} %s\n" "$1"; }

FAILURES=0
SKIPS=0
if command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  COMPOSE="docker compose"
fi
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
REPLICA_IDS=(1 2 3 4)
GATEWAY_WS="ws://localhost:8080/ws"
GATEWAY_HTTP="http://localhost:8080"
PYTHON_BIN="${PYTHON_BIN:-}"
SMOKE_BOOT_TIMEOUT_SEC="${SMOKE_BOOT_TIMEOUT_SEC:-90}"

resolve_python_cmd() {
  if [ -n "$PYTHON_BIN" ]; then
    if [ -x "$PYTHON_BIN" ]; then
      echo "$PYTHON_BIN"
      return
    fi
    # Handle Windows-style path values in Git Bash, e.g. c:/path/python.exe.
    if command -v "$PYTHON_BIN" >/dev/null 2>&1; then
      echo "$PYTHON_BIN"
      return
    fi
  fi

  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi

  echo ""
}

PYTHON_CMD="$(resolve_python_cmd)"

python_has_module() {
  local module="$1"
  if [ -z "$PYTHON_CMD" ]; then
    return 1
  fi
  "$PYTHON_CMD" -c "import ${module}" >/dev/null 2>&1
}

# --- Replica HTTP helper (internal network) ---------------------------------
replica_http() {
  local idx="$1" path="$2"
  $COMPOSE exec -T "replica${idx}" sh -lc "curl -sf http://localhost:8081${path}" 2>/dev/null || true
}

status_state_from_json() {
  local status_json="$1"
  local parsed=""

  if command -v jq >/dev/null 2>&1; then
    parsed=$(echo "$status_json" | jq -r '.state // empty' 2>/dev/null || true)
    if [ -n "$parsed" ]; then
      echo "$parsed"
      return
    fi
  fi

  if [ -n "$PYTHON_CMD" ]; then
    parsed=$(STATUS_JSON="$status_json" "$PYTHON_CMD" - <<'PYEOF'
import json
import os

raw = os.environ.get("STATUS_JSON", "")
try:
    obj = json.loads(raw)
    value = obj.get("state", "")
    if isinstance(value, str):
        print(value)
except Exception:
    pass
PYEOF
  2>/dev/null || true)
    if [ -n "$parsed" ]; then
      echo "$parsed"
      return
    fi
  fi

  echo "$status_json" | sed -n 's/.*"state"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

status_log_len_from_json() {
  local status_json="$1"
  local default_value="${2:-0}"
  local parsed=""

  if command -v jq >/dev/null 2>&1; then
    parsed=$(echo "$status_json" | jq ".logLength // ${default_value}" 2>/dev/null || true)
    if [[ "$parsed" =~ ^-?[0-9]+$ ]]; then
      echo "$parsed"
      return
    fi
  fi

  if [ -n "$PYTHON_CMD" ]; then
    parsed=$(STATUS_JSON="$status_json" DEFAULT_VALUE="$default_value" "$PYTHON_CMD" - <<'PYEOF'
import json
import os

raw = os.environ.get("STATUS_JSON", "")
default = int(os.environ.get("DEFAULT_VALUE", "0"))
try:
    obj = json.loads(raw)
    value = obj.get("logLength", default)
    print(int(value))
except Exception:
    print(default)
PYEOF
  2>/dev/null || true)
    if [[ "$parsed" =~ ^-?[0-9]+$ ]]; then
      echo "$parsed"
      return
    fi
  fi

  parsed=$(echo "$status_json" | sed -n 's/.*"logLength"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n1)
  echo "${parsed:-$default_value}"
}

compose_project_name() {
  if command -v jq >/dev/null 2>&1; then
    local name
    name=$(cd "$REPO_ROOT" && $COMPOSE config --format json 2>/dev/null | jq -r '.name // empty' 2>/dev/null || true)
    if [ -n "$name" ]; then
      echo "$name"
      return
    fi
  fi
  basename "$REPO_ROOT" | tr '[:upper:]' '[:lower:]'
}

# --- Step 1: Bring up the stack ----------------------------------------------
step1() {
  INFO "Step 1: docker compose up --build -d"
  cd "$REPO_ROOT"
  $COMPOSE up --build -d 2>&1 | tail -5

  INFO "Waiting for all replicas and gateway to pass healthchecks (up to ${SMOKE_BOOT_TIMEOUT_SEC}s)..."
  local deadline=$(( $(date +%s) + SMOKE_BOOT_TIMEOUT_SEC ))
  local healthy=0
  while [ $(date +%s) -lt $deadline ]; do
    healthy=0
    for i in "${REPLICA_IDS[@]}"; do
      if [ -n "$(replica_http "$i" "/health")" ]; then
        healthy=$((healthy+1))
      fi
    done

    local gw_ok=0
    if curl -sf "${GATEWAY_HTTP}/health" -o /dev/null 2>/dev/null; then gw_ok=1; fi

    if [ "$healthy" -eq "${#REPLICA_IDS[@]}" ] && [ "$gw_ok" -eq 1 ]; then
      PASS "Step 1: all $(( ${#REPLICA_IDS[@]} + 1 )) services healthy"
      return 0
    fi
    sleep 2
  done

  FAIL "Step 1: timed out waiting for healthchecks (replicas_healthy=$healthy, gw=$gw_ok)"
  return 1
}

# --- WS helper ---------------------------------------------------------------
# Sends one message to the WebSocket and captures output for N seconds.
# Prints received frames to stdout. Tries websocat, then wscat, then python.
ws_send_recv() {
  local url="$1" msg="$2" timeout_sec="$3"

  if command -v websocat &>/dev/null; then
    echo "$msg" | timeout "$timeout_sec" websocat --no-close -n1 "$url" 2>/dev/null || true
  elif command -v wscat &>/dev/null; then
    wscat --connect "$url" --execute "$msg" --wait "$((timeout_sec*1000))" 2>/dev/null || true
  elif python_has_module websockets; then
    "$PYTHON_CMD" - <<PYEOF
import asyncio
import websockets

async def run():
    async with websockets.connect("$url") as ws:
        await ws.send('''$msg''')
        try:
            while True:
                message = await asyncio.wait_for(ws.recv(), timeout=$timeout_sec)
                print(message)
        except asyncio.TimeoutError:
            pass

asyncio.run(run())
PYEOF
  else
    echo "NO_WS_CLIENT"
  fi
}

# --- Step 2: STROKE_DRAW -> STROKE_COMMITTED ---------------------------------
SMOKE_STROKE_ID="smoke-$(date +%s%N | md5sum | head -c8)"
STROKE_DRAW_MSG='{"type":"STROKE_DRAW","payload":{"strokeId":"'"$SMOKE_STROKE_ID"'","points":[{"x":10,"y":10},{"x":20,"y":20}],"colour":"#ff0000","width":3,"strokeTool":"pen"}}'

step2() {
  INFO "Step 2: STROKE_DRAW -> STROKE_COMMITTED (strokeId=$SMOKE_STROKE_ID)"
  if ! command -v websocat &>/dev/null && ! command -v wscat &>/dev/null; then
    if ! python_has_module websockets; then
      SKIP "Step 2: no WebSocket client available (install websocat or wscat)"
      return 0
    fi
  fi

  local output
  output=$(ws_send_recv "$GATEWAY_WS" "$STROKE_DRAW_MSG" 4)
  if echo "$output" | grep -q "STROKE_COMMITTED"; then
    PASS "Step 2: STROKE_COMMITTED received within 3s"
  else
    FAIL "Step 2: STROKE_COMMITTED not received (output: $(echo "$output" | head -3))"
  fi
}

# --- Step 3: Find leader and kill it -----------------------------------------
KILLED_CONTAINER=""
KILLED_REPLICA_IDX=""

step3() {
  INFO "Step 3: Identify current leader and kill its container"
  local leader_id=""

  for i in "${REPLICA_IDS[@]}"; do
    local status_json
    status_json=$(replica_http "$i" "/status")
    local state
    state=$(status_state_from_json "$status_json")
    if [ "$state" = "LEADER" ]; then
      leader_id="replica${i}"
      break
    fi
  done

  if [ -z "$leader_id" ]; then
    INFO "No leader yet, waiting up to 10s..."
    local deadline=$(( $(date +%s) + 10 ))
    while [ $(date +%s) -lt $deadline ]; do
      for i in "${REPLICA_IDS[@]}"; do
        local status_json
        status_json=$(replica_http "$i" "/status")
        local state
        state=$(status_state_from_json "$status_json")
        if [ "$state" = "LEADER" ]; then
          leader_id="replica${i}"
          break 2
        fi
      done
      sleep 0.5
    done
  fi

  if [ -z "$leader_id" ]; then
    FAIL "Step 3: could not identify leader within 10s"
    return 1
  fi

  local project
  project=$(compose_project_name)
  KILLED_CONTAINER="${project}-${leader_id}-1"
  KILLED_REPLICA_IDX="${leader_id: -1}"

  INFO "Leader is $leader_id, killing container $KILLED_CONTAINER"
  docker stop "$KILLED_CONTAINER" >/dev/null
  PASS "Step 3: killed container $KILLED_CONTAINER (was $leader_id)"
}

# --- Step 4: New leader within 3s --------------------------------------------
NEW_LEADER_ID=""

step4() {
  INFO "Step 4: Polling for new leader (up to 3s)"
  local deadline=$(( $(date +%s) + 3 ))
  while [ $(date +%s) -lt $deadline ]; do
    for i in "${REPLICA_IDS[@]}"; do
      [ "$i" = "$KILLED_REPLICA_IDX" ] && continue
      local status_json
      status_json=$(replica_http "$i" "/status")
      local state
      state=$(status_state_from_json "$status_json")
      if [ "$state" = "LEADER" ]; then
        NEW_LEADER_ID="replica${i}"
        PASS "Step 4: new leader elected - replica${i} within 3s"
        return 0
      fi
    done
    sleep 0.1
  done

  FAIL "Step 4: no new leader elected within 3s"
  return 1
}

# --- Step 5: STROKE_DRAW after failover --------------------------------------
step5() {
  INFO "Step 5: STROKE_DRAW after failover -> STROKE_COMMITTED within 3s"
  if ! command -v websocat &>/dev/null && ! command -v wscat &>/dev/null; then
    if ! python_has_module websockets; then
      SKIP "Step 5: no WebSocket client available"
      return 0
    fi
  fi

  local stroke_id="smoke-post-failover-$(date +%s%N | md5sum | head -c8)"
  local msg='{"type":"STROKE_DRAW","payload":{"strokeId":"'"$stroke_id"'","points":[{"x":30,"y":30}],"colour":"#0000ff","width":2,"strokeTool":"pen"}}'
  local output
  output=$(ws_send_recv "$GATEWAY_WS" "$msg" 4)
  if echo "$output" | grep -q "STROKE_COMMITTED"; then
    PASS "Step 5: STROKE_COMMITTED received after failover"
  else
    FAIL "Step 5: STROKE_COMMITTED not received after failover (output: $(echo "$output" | head -3))"
  fi
}

# --- Step 6: Restart the stopped container -----------------------------------
step6() {
  INFO "Step 6: Restarting $KILLED_CONTAINER"
  if [ -z "$KILLED_CONTAINER" ]; then
    FAIL "Step 6: no container was killed - skipping"
    return 1
  fi
  docker start "$KILLED_CONTAINER" >/dev/null
  PASS "Step 6: restarted $KILLED_CONTAINER"
}

# --- Step 7: Catch-up verification -------------------------------------------
step7() {
  INFO "Step 7: Waiting for restarted replica to stabilize (FOLLOWER/LEADER) and catch up log (up to 10s)"
  if [ -z "$KILLED_REPLICA_IDX" ]; then
    FAIL "Step 7: unknown killed replica index"
    return 1
  fi

  local leader_log_len=0
  if [ -n "$NEW_LEADER_ID" ]; then
    local leader_idx="${NEW_LEADER_ID#replica}"
    leader_log_len=$(status_log_len_from_json "$(replica_http "$leader_idx" "/status")" "0")
  fi
  INFO "Leader logLength=$leader_log_len; waiting for replica$KILLED_REPLICA_IDX to catch up"

  local deadline=$(( $(date +%s) + 10 ))
  while [ $(date +%s) -lt $deadline ]; do
    local status_json
    status_json=$(replica_http "$KILLED_REPLICA_IDX" "/status")
    local state log_len
    state=$(status_state_from_json "$status_json")
    log_len=$(status_log_len_from_json "$status_json" "-1")

    if { [ "$state" = "FOLLOWER" ] || [ "$state" = "LEADER" ]; } && [ "$log_len" -ge "$leader_log_len" ]; then
      PASS "Step 7: replica$KILLED_REPLICA_IDX stabilized as $state with logLength=$log_len (leader baseline=$leader_log_len)"
      return 0
    fi
    sleep 0.2
  done

  FAIL "Step 7: replica$KILLED_REPLICA_IDX did not stabilize/catch up in 10s (state=$state, logLength=$log_len, leader baseline=$leader_log_len)"
  return 1
}

# --- Step 8: Tear down --------------------------------------------------------
step8() {
  INFO "Step 8: docker compose down"
  cd "$REPO_ROOT"
  $COMPOSE down -v 2>&1 | tail -3
  PASS "Step 8: cluster torn down"
}

# --- Main --------------------------------------------------------------------
main() {
  echo "========================================"
  echo "  MiniRAFT Smoke Test"
  echo "========================================"

  trap 'step8 2>/dev/null || true' EXIT

  step1 || { FAIL "Step 1 fatal - aborting"; exit 1; }
  step2
  step3 || { FAIL "Step 3 fatal - aborting failover steps"; FAILURES=$((FAILURES+4)); step8; exit 1; }
  step4 || { FAIL "Step 4 fatal - aborting post-failover steps"; FAILURES=$((FAILURES+3)); step8; exit 1; }
  step5
  step6 || true
  step7

  echo "========================================"
  if [ "$FAILURES" -eq 0 ]; then
    if [ "$SKIPS" -gt 0 ]; then
      printf "${GREEN}ALL STEPS PASSED${NC} (${YELLOW}%s skipped${NC})\n" "$SKIPS"
    else
      printf "${GREEN}ALL STEPS PASSED${NC}\n"
    fi
    exit 0
  else
    if [ "$SKIPS" -gt 0 ]; then
      printf "${RED}$FAILURES STEP(S) FAILED${NC} (${YELLOW}%s skipped${NC})\n" "$SKIPS"
    else
      printf "${RED}$FAILURES STEP(S) FAILED${NC}\n"
    fi
    exit 1
  fi
}

main "$@"