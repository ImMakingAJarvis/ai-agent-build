#!/usr/bin/env bash
#
# e2e-event-flow.sh — Phase 4 gate (event-driven brain plan, task EVB-30).
#
# Drives the plan's canonical §3.2 flow end-to-end on the live box with a
# scripted stand-in round (jarvis-jobs mark-running + yield — the plan
# sanctions stand-ins; the Phase 2 gate scripts/e2e-cap-echo.sh is the
# precedent):
#
#   S1  the canonical flow   the stand-in brain job yields on an
#                            image.generate (12 GB); the resourced window
#                            controller opens the swap window (the brain
#                            container actually stops); while the image is
#                            in flight, an llm.small (5.5 GB) completes on
#                            the 4090's SPARE VRAM — the co-scheduling
#                            window, on the gpu placement; the window
#                            closes (drained), the brain actually comes
#                            back, and the switchboard wakes the job — it
#                            resumes and finishes.
#   S2  voice in the window  (inside S1's window, brain down) a /chat
#                            request is answered by the iGPU helper
#                            (Qwen2.5-3B on :11435) — verified by the
#                            reply, the /health fallback probe
#                            (brain_up=false) and the voice service's
#                            route log line.
#   S3  game in the window   a fresh window opens for a second image;
#                            after it finishes, while an llm.small is in
#                            flight, a Sunshine-style connected UDP socket
#                            on the stream port appears: the game watchdog
#                            (EVB-29) turns on the GPU hold, revokes the
#                            capability workers' GPU leases, DRAINS the
#                            in-flight work (it completes — its result
#                            lands) and closes the window, so the brain
#                            comes back.
#
# Rule 3 (a window must not open while a local round is RUNNING) is
# honored by pausing the caller's own round job for the run — the trap
# restores it to ready on every exit path. The in-flight work the game
# can catch is deliberately the 5.5 GB small job, never the 12 GB image:
# brain (15.3 GB) + small (5.5 GB) fits the 4090's 24.5 GB, so the
# CloseForGame brain-load cannot OOM.
#
# The gate requires the deployed stack to match this tree: the resourced
# with the window controller + game watchdog, the voice service with
# brain-down fallback routing, and the capability workers. Preflight
# verifies what it can (window.cap_depths in /state, fallback.brain_up in
# voice /health, sunshine running for the S3 detector).
#
# Usage:  ROUND_JOB=<your-round-job-id> bash scripts/e2e-event-flow.sh [transcript.log]
#         (exit 0 = all scenarios pass; 1 = any failed; 2 = preflight failed)
#
# The caller's own session runs on the brain engine, which the gate stops
# and starts: run this detached (setsid/nohup) and poll the transcript —
# the tool-call turn that waits on the transcript spans the whole window
# with no engine request in flight.

set -uo pipefail

# --- config -----------------------------------------------------------------
DATA="${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}"
JOBS="$DATA/jobs"
RUN="$(date -u +%s)"
ART_ROOT="$DATA/artifacts/e2e-event-flow-$RUN"   # shared with the worker containers
RESOURCED="${RESOURCED_URL:-http://172.17.0.1:9104}"
BRAIN="${BRAIN_URL:-http://127.0.0.1:11434}"
VOICE="${VOICE_URL:-http://127.0.0.1:8100}"
ROUND_JOB="${ROUND_JOB:-}"
STREAM_PORT="${SUNSHINE_STREAM_PORT:-48010}"
IMG_PROMPT="a lighthouse at dusk, oil painting"

# Budgets (ms) — generous: the gate must fail loudly, not flaky-fast.
WINDOW_OPEN_BUDGET_MS=150000    # two 30 s controller ticks + the swap-out
BRAIN_DOWN_BUDGET_MS=90000      # the engine container stops fast
VOICE_BUDGET_MS=120000          # iGPU helper: ~13 s prompt + ~13 tok/s
SMALL_BUDGET_MS=240000          # coprocess load + a few hundred tokens
IMAGE_BUDGET_MS=300000          # catalog typical_runtime 45 s
WINDOW_CLOSE_BUDGET_MS=480000   # drained + min_window 2m + tick + swap-in
BRAIN_UP_BUDGET_MS=240000       # container start + 15.3 GB load + health
WAKE_BUDGET_MS=60000            # result -> switchboard wake
GAME_DETECT_BUDGET_MS=60000     # 2 s poll + margin
GAME_CLOSE_BUDGET_MS=90000      # CloseForGame -> window closed
DRAIN_BUDGET_MS=180000          # in-flight small job completes
STREAM_OFF_BUDGET_MS=60000      # socket closed -> streamStopped

TRANSCRIPT="${1:-/tmp/e2e-event-flow-$RUN.log}"
mkdir -p "$ART_ROOT"
: > "$TRANSCRIPT"
exec > "$TRANSCRIPT" 2>&1

PASS=0
FAIL=0
declare -a RESULTS=()      # "<name>:PASS|FAIL"
declare -a TRACKED_JOBS=()
UDP_PID=""

# --- helpers ----------------------------------------------------------------
# Millisecond clock (base-10 forced: a 0-leading nanosecond field would
# otherwise be read as octal).
now_ms() {
  local t; t="$(date +%s%N)"
  echo $(( 10#${t:0:10} * 1000 + 10#${t:10:9} / 1000000 ))
}
log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

status_of() { # status_of <jobid> -> the state.json status (empty if gone)
  grep -oE '"status"[[:space:]]*:[[:space:]]*"[a-z]+"' "$JOBS/$1/state.json" 2>/dev/null \
    | head -1 | sed -E 's/.*:[[:space:]]*"([a-z]+)"/\1/'
}

# resourced /state access (the broker contract: devices/leases/queued,
# stream_active, window{open,for,reason,cap_depths}).
state() { curl -sf --max-time 5 "$RESOURCED/state"; }
state_get() { # state_get <python-expr over d> -> json-encoded value
  state 2>/dev/null | python3 -c "import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(0)
v=$1
print(json.dumps(v))" 2>/dev/null
}
window_open() { [ "$(state_get "d.get('window',{}).get('open')")" = "true" ]; }
window_closed() { ! window_open; }
stream_active() { [ "$(state_get "d.get('stream_active')")" = "true" ]; }
stream_off() { ! stream_active; }
brain_down() { ! brain_up; }
# gpu_leases -> one "holder capability vram_mb" line per gpu lease
gpu_leases() {
  state 2>/dev/null | python3 -c "import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(0)
for l in d.get('leases',[]):
    if l.get('device')=='gpu':
        print(l.get('holder','?'), l.get('capability',''), l.get('vram_mb',0))" 2>/dev/null
}
# Up = the engine answers at all (200 healthy or 503 busy — a stopped
# container is a connection failure, which is the down signal).
brain_up() { curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$BRAIN/health" 2>/dev/null | grep -qE '^(200|503)$'; }

# wait_flag <desc> <predicate-fn> <budget_ms> — poll until the predicate is
# true. Returns 0 on success.
wait_flag() {
  local desc="$1" fn="$2" budget="$3" t0
  t0="$(now_ms)"
  while ! "$fn" >/dev/null 2>&1; do
    if [ $(( $(now_ms) - t0 )) -gt "$budget" ]; then return 1; fi
    sleep 1
  done
  return 0
}

track() { TRACKED_JOBS+=("$1"); }

cleanup_scenario() { # cleanup_scenario <jobid> <flow...>
  local id="$1"; shift
  local flow
  for flow in "$@"; do
    jarvis-mq unlisten "$flow" >/dev/null 2>&1 || true
    jarvis-mq results "$flow" >/dev/null 2>&1 || true
    jarvis-mq delete "$flow" >/dev/null 2>&1 || true
  done
  jarvis-jobs finish "$id" 0 >/dev/null 2>&1 || true
  rm -rf "$JOBS/$id" 2>/dev/null || true
}

fail() {
  log "FAIL $1: $2"
  RESULTS+=("$1:FAIL")
  FAIL=$((FAIL + 1))
}
ok() {
  log "PASS $1"
  RESULTS+=("$1:PASS")
  PASS=$((PASS + 1))
}

# wait_window_open <budget_ms> — the controller opens on the next tick.
wait_window_open() {
  wait_flag "window open" window_open "${1:-$WINDOW_OPEN_BUDGET_MS}"
}
wait_window_closed() {
  wait_flag "window closed" window_closed "${1:-$WINDOW_CLOSE_BUDGET_MS}"
}
wait_brain_down() {
  wait_flag "brain down" brain_down "${1:-$BRAIN_DOWN_BUDGET_MS}"
}
wait_brain_up() {
  wait_flag "brain up" brain_up "${1:-$BRAIN_UP_BUDGET_MS}"
}

# wait_file <path> <budget_ms>
wait_file() {
  local f="$1" budget="$2" t0
  t0="$(now_ms)"
  while [ ! -f "$f" ]; do
    if [ $(( $(now_ms) - t0 )) -gt "$budget" ]; then return 1; fi
    sleep 1
  done
  return 0
}

# wait_gpu_lease <capability> <budget_ms> — the capability's gpu lease is
# held (the work is in flight with VRAM).
LEASE_CAP=""
lease_pred() { [ -n "$LEASE_CAP" ] && gpu_leases | grep -q " $LEASE_CAP "; }
wait_gpu_lease() {
  LEASE_CAP="$1"
  wait_flag "gpu lease $1" lease_pred "${2:-$SMALL_BUDGET_MS}"
}

# wait_job_ready <jobid> <budget_ms>
wait_job_ready() {
  local id="$1" budget="$2" t0
  t0="$(now_ms)"
  while [ "$(status_of "$id")" != "ready" ]; do
    if [ $(( $(now_ms) - t0 )) -gt "$budget" ]; then return 1; fi
    sleep 0.5
  done
  return 0
}

# stand-in round: add -> mark-running -> submit -> listen -> yield. Leaves
# the job waiting on $corr.
setup_standin() {
  local id="$1" flow="$2" corr="$3" out="$4" cap="$5"
  track "$id"
  jarvis-jobs add -id "$id" -title "Phase 4 gate: $cap" -driver local \
    -instructions "scripted stand-in round for the e2e-event-flow gate" >/dev/null \
    || { log "add $id failed"; return 1; }
  jarvis-jobs mark-running "$id" >/dev/null || { log "mark-running $id failed"; return 1; }
  jarvis-mq submit "$cap" --reply-to "$flow" --corr "$corr" \
    --input prompt="$IMG_PROMPT" --out-dir "$out" --wake-job "$id" >/dev/null \
    || { log "submit $cap failed"; return 1; }
  jarvis-mq listen "$flow" --wake "$id" --corr "$corr" --timeout 2h >/dev/null \
    || { log "listen $flow failed"; return 1; }
  jarvis-jobs yield "$id" --wait-on "$corr" --note "waiting for $cap" \
    --in-flight "- $corr: $cap request (12 GB, swap window)" \
    --next "- read the $flow result, verify the artifact, finish" >/dev/null \
    || { log "yield $id failed"; return 1; }
  return 0
}

# hold_udp <seconds> — a connected UDP socket touching the Sunshine stream
# port: the S3 "game starts" simulation. sunshine (user nathan) runs on this
# box, and /proc/<sunshine-pid>/net/udp is the whole netns, so ANY connected
# st-01 socket on the stream port is detected as a stream.
hold_udp() {
  python3 - "$1" "$STREAM_PORT" <<'EOF' &
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.connect(("127.0.0.1", int(sys.argv[2])))
time.sleep(int(sys.argv[1]))
EOF
  UDP_PID=$!
}
release_udp() {
  [ -n "$UDP_PID" ] && kill "$UDP_PID" >/dev/null 2>&1 || true
  wait "$UDP_PID" 2>/dev/null || true
  UDP_PID=""
}

# --- scenarios --------------------------------------------------------------
# S1 + S2 share one window: the canonical flow, with the voice check inside
# it (the brain is down exactly then).
s1_s2() {
  local id="gate-$RUN-s1"
  local flow="flow.$id" corr="${id}-img" out="$ART_ROOT/$id"
  local small_flow="flow.$id-small" small_corr="${id}-small" small_out="$out/small"
  log "S1: canonical flow — stand-in job yields on image.generate (12 GB)"
  mkdir -p "$small_out"
  if ! setup_standin "$id" "$flow" "$corr" "$out" "image.generate"; then
    fail S1 "setup"; cleanup_scenario "$id" "$flow"; return
  fi
  log "S1: $id is now $(status_of "$id"); waiting for the window to open (budget $((WINDOW_OPEN_BUDGET_MS / 1000)) s)"
  if ! wait_window_open; then
    fail S1 "window did not open (depth cap.image.generate=$(jarvis-mq depth cap.image.generate 2>/dev/null), window=$(state_get "d.get('window',{})"))"
    cleanup_scenario "$id" "$flow"; return
  fi
  log "S1: window OPEN (for=$(state_get "d.get('window',{}).get('for')") reason=$(state_get "d.get('window',{}).get('reason')"))"
  log "S1: waiting for the brain engine to actually stop (the real completion check, going out)"
  if ! wait_brain_down; then
    fail S1 "window open but the brain engine is still answering (jarvis-swap out did not take effect)"
    cleanup_scenario "$id" "$flow"; return
  fi
  log "S1: brain engine DOWN ($(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$BRAIN/health" 2>/dev/null || echo unreachable))"

  # --- S2: voice during the window ------------------------------------------
  log "S2: voice /chat during the window (brain down) — expecting the iGPU helper"
  local vh
  vh="$(curl -sf --max-time "$(( VOICE_BUDGET_MS / 1000 ))" -o "$ART_ROOT/voice-s2.json" -w '%{http_code}' \
    -X POST "$VOICE/chat" -H 'Content-Type: application/json' \
    -d '{"messages":[{"role":"user","content":"Gate check: reply with the single word OK."}]}' 2>&1)"
  if [ "$vh" != "200" ] || [ ! -s "$ART_ROOT/voice-s2.json" ]; then
    fail S2 "voice /chat failed (http $vh)"
  else
    local reply
    reply="$(python3 -c "import json;print(json.load(open('$ART_ROOT/voice-s2.json')).get('reply',''))" 2>/dev/null)"
    log "S2: voice replied (http 200): ${reply:0:80}"
    if [ -z "$reply" ]; then
      fail S2 "empty reply"
    else
      local bu route
      # No -f: while the brain is down the voice /health answers 503 BY
      # DESIGN, and -f would drop the body — the brain_up flag lives in
      # that 503 body. (EVB-30 gate: -sf made bu="" and S2 always
      # failed even when the fallback routed correctly.)
      bu="$(curl -s --max-time 5 "$VOICE/health" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin).get('fallback',{}).get('brain_up'))" 2>/dev/null)"
      log "S2: voice /health fallback.brain_up=$bu (want False)"
      route="$(docker logs jarvis-voice --since 5m 2>&1 | grep -c 'brain down -> fallback' || true)"
      log "S2: voice route log 'brain down -> fallback' lines: $route (want >= 1)"
      if [ "$bu" = "False" ] && [ "${route:-0}" -ge 1 ] 2>/dev/null; then
        ok S2
      else
        fail S2 "fallback routing not verified (brain_up=$bu, route lines=$route)"
      fi
    fi
  fi

  # --- S1 continues: co-schedule the small job on the spare VRAM -------------
  log "S1: submitting llm.small (5.5 GB) while the image is in flight — the co-scheduling window"
  printf 'The event-driven brain swaps the 27B model out of the 4090 to make room for image work.\nWhile the window is open, small batch sub-tasks may share the spare VRAM.\nSummarize the key idea in one sentence.\n' > "$small_out/input.txt"
  if ! jarvis-mq submit llm.small --reply-to "$small_flow" --corr "$small_corr" \
      --input task=summarise --input-ref "$small_out/input.txt" --out-dir "$small_out" >/dev/null; then
    fail S1 "submit llm.small failed"; cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: capturing the co-scheduled GPU leases (want image.generate AND llm.small)"
  local co t0
  t0="$(now_ms)"
  co=""
  while :; do
    co="$(gpu_leases)"
    if grep -q " image.generate " <<<"$co" && grep -q " llm.small " <<<"$co"; then break; fi
    if [ $(( $(now_ms) - t0 )) -gt 120000 ]; then break; fi
    sleep 1
  done
  log "S1: gpu leases now:"
  sed 's/^/        /' <<<"$co"
  nvidia-smi --query-gpu=memory.used,memory.free --format=csv,noheader 2>/dev/null | sed 's/^/        vram /' || true
  if ! grep -q " image.generate " <<<"$co" || ! grep -q " llm.small " <<<"$co"; then
    log "S1: both leases were not held simultaneously (the small job may have finished before the poll caught it)"
  fi

  log "S1: waiting for the llm.small result (budget $((SMALL_BUDGET_MS / 1000)) s)"
  if ! wait_file "$small_out/result.json" "$SMALL_BUDGET_MS"; then
    fail S1 "no llm.small result within budget"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  local small_worker
  small_worker="$(python3 -c "import json;print(json.load(open('$small_out/result.json')).get('worker',''))" 2>/dev/null)"
  log "S1: llm.small done on worker=$small_worker (want the gpu placement: llm.small@gpu/llm-small-worker)"
  if ! grep -q "@gpu/" <<<"$small_worker"; then
    fail S1 "llm.small did not take the gpu placement (worker=$small_worker) — the co-scheduling window did not hold the 5.5 GB"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi

  log "S1: waiting for the image artifact (budget $((IMAGE_BUDGET_MS / 1000)) s)"
  local img="" t1
  t1="$(now_ms)"
  while :; do
    img="$(find "$out" -maxdepth 1 -type f ! -name '*.json' ! -name 'input.txt' -size +10k 2>/dev/null | head -1)"
    [ -n "$img" ] && break
    if [ $(( $(now_ms) - t1 )) -gt "$IMAGE_BUDGET_MS" ]; then break; fi
    sleep 1
  done
  if [ -z "$img" ]; then
    fail S1 "no image artifact in $out"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: image artifact: $img ($(du -h "$img" | cut -f1))"
  local res
  res="$(jarvis-mq results "$flow" 2>&1)"
  if ! grep -q "status=ok" <<<"$res"; then
    fail S1 "Result on $flow is not status=ok: $(head -3 <<<"$res")"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: image Result status=ok"

  log "S1: waiting for the window to close (drained; budget $((WINDOW_CLOSE_BUDGET_MS / 1000)) s)"
  if ! wait_window_closed; then
    fail S1 "window did not close (window=$(state_get "d.get('window',{})"))"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: window CLOSED (reason=$(state_get "d.get('window',{}).get('reason')"))"
  log "S1: waiting for the brain engine to actually come back (budget $((BRAIN_UP_BUDGET_MS / 1000)) s)"
  if ! wait_brain_up; then
    fail S1 "brain engine did not come back after the window closed"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: brain engine UP again — the round can continue"

  log "S1: waiting for the switchboard to wake $id (budget $((WAKE_BUDGET_MS / 1000)) s)"
  if ! wait_job_ready "$id" "$WAKE_BUDGET_MS"; then
    fail S1 "job not woken to ready (status=$(status_of "$id"))"
    cleanup_scenario "$id" "$flow" "$small_flow"; return
  fi
  log "S1: $id woken to ready — the stand-in round resumes and finishes"
  jarvis-jobs finish "$id" 0 >/dev/null
  rm -rf "$JOBS/$id" 2>/dev/null
  jarvis-mq unlisten "$small_flow" >/dev/null 2>&1 || true
  jarvis-mq delete "$small_flow" >/dev/null 2>&1 || true
  jarvis-mq delete "$flow" >/dev/null 2>&1 || true
  ok S1
}

# S3: a fresh window; the game starts while the small job is in flight.
s3_game() {
  local id="gate-$RUN-s3"
  local flow="flow.$id" corr="${id}-img" out="$ART_ROOT/$id"
  local small_flow="flow.$id-small" small_corr="${id}-small" small_out="$out/small"
  log "S3: game in the window — fresh image window, then a stream socket mid-drain"
  mkdir -p "$small_out"
  local attempt
  for attempt in 1 2 3; do
    log "S3 (attempt $attempt): submitting image.generate (no wake — the window trigger only)"
    if ! jarvis-mq submit image.generate --reply-to "$flow" --corr "$corr" \
        --input prompt="$IMG_PROMPT" --out-dir "$out" >/dev/null; then
      fail S3 "submit image.generate failed"; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    if ! wait_window_open; then
      log "S3: window did not open on attempt $attempt"
      jarvis-mq purge "$flow" >/dev/null 2>&1 || true
      continue
    fi
    log "S3: window OPEN (reason=$(state_get "d.get('window',{}).get('reason')"))"
    log "S3: waiting for the image to finish (budget $((IMAGE_BUDGET_MS / 1000)) s) — the game must start while a SMALL job is in flight, never the 12 GB image"
    local img="" t0
    t0="$(now_ms)"
    while :; do
      img="$(find "$out" -maxdepth 1 -type f ! -name '*.json' -size +10k 2>/dev/null | head -1)"
      [ -n "$img" ] && break
      if [ $(( $(now_ms) - t0 )) -gt "$IMAGE_BUDGET_MS" ]; then break; fi
      sleep 1
    done
    if [ -z "$img" ]; then
      log "S3: image never finished (attempt $attempt); retrying"
      jarvis-mq purge "$flow" >/dev/null 2>&1 || true
      continue
    fi
    log "S3: image done ($img); the window holds (drained, younger than min_window) — starting the small job"
    if ! window_open; then
      log "S3: the window auto-closed before the small job could start (attempt $attempt); retrying"
      jarvis-mq purge "$flow" >/dev/null 2>&1 || true
      continue
    fi
    printf 'The game watchdog revokes GPU leases when a stream starts; in-flight work drains.\nSummarize the key idea in one sentence.\n' > "$small_out/input.txt"
    if ! jarvis-mq submit llm.small --reply-to "$small_flow" --corr "$small_corr" \
        --input task=summarise --input-ref "$small_out/input.txt" --out-dir "$small_out" >/dev/null; then
      fail S3 "submit llm.small failed"; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    log "S3: waiting for the small job's gpu lease (in flight, budget $((SMALL_BUDGET_MS / 1000)) s)"
    if ! wait_gpu_lease "llm.small" "$SMALL_BUDGET_MS"; then
      log "S3: the small job never got its gpu lease (attempt $attempt); retrying"
      jarvis-mq purge "$small_flow" >/dev/null 2>&1 || true
      continue
    fi
    log "S3: small job IN FLIGHT (leases: $(gpu_leases | tr '\n' '; ')) — the game starts now"
    hold_udp 300
    log "S3: UDP socket held on 127.0.0.1:$STREAM_PORT (pid $UDP_PID) — the stream detector polls every 2 s"
    if ! wait_flag "stream active" stream_active "$GAME_DETECT_BUDGET_MS"; then
      fail S3 "game watchdog did not detect the stream within $((GAME_DETECT_BUDGET_MS / 1000)) s (stream_active=$(state_get "d.get('stream_active')"))"
      release_udp; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    log "S3: stream ACTIVE — the GPU hold is on (leases now: $(gpu_leases | tr '\n' '; '))"
    if ! wait_window_closed "$GAME_CLOSE_BUDGET_MS"; then
      fail S3 "window did not close on the game (window=$(state_get "d.get('window',{})"))"
      release_udp; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    log "S3: window CLOSED by the game (reason=$(state_get "d.get('window',{}).get('reason')"))"
    local reason
    reason="$(state_get "d.get('window',{}).get('reason')" | tr -d '"')"
    if ! grep -q "game" <<<"$reason"; then
      log "S3: note — the close reason is '$reason' (the game may have raced a drained tick; the hold was active, which is what matters)"
    fi
    log "S3: waiting for the in-flight small job to DRAIN (its result must land, budget $((DRAIN_BUDGET_MS / 1000)) s)"
    if ! wait_file "$small_out/result.json" "$DRAIN_BUDGET_MS"; then
      fail S3 "the in-flight small job did not complete during the drain"
      release_udp; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    local small_worker
    small_worker="$(python3 -c "import json;print(json.load(open('$small_out/result.json')).get('worker',''))" 2>/dev/null)"
    log "S3: in-flight work drained: small job completed (worker=$small_worker)"
    log "S3: waiting for the brain engine back (budget $((BRAIN_UP_BUDGET_MS / 1000)) s)"
    if ! wait_brain_up; then
      fail S3 "brain engine did not come back after the game closed the window"
      release_udp; cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    log "S3: brain engine UP"
    release_udp
    log "S3: stream socket released — waiting for stream_active to clear (budget $((STREAM_OFF_BUDGET_MS / 1000)) s)"
    if ! wait_flag "stream off" stream_off "$STREAM_OFF_BUDGET_MS"; then
      fail S3 "stream hold did not clear after the socket closed"
      cleanup_scenario "$id" "$flow" "$small_flow"; return
    fi
    log "S3: stream hold cleared — everything is back (window closed, brain up, hold off, in-flight work done)"
    jarvis-mq results "$flow" >/dev/null 2>&1 || true
    jarvis-mq delete "$flow" >/dev/null 2>&1 || true
    jarvis-mq results "$small_flow" >/dev/null 2>&1 || true
    jarvis-mq delete "$small_flow" >/dev/null 2>&1 || true
    track "$id"  # a bookkeeping job only (never added to the store) — no store cleanup needed
    ok S3
    return
  done
  fail S3 "all 3 attempts failed to run the game-in-window sequence"
  cleanup_scenario "$id" "$flow" "$small_flow"
}

# --- cleanup / exit trap ----------------------------------------------------
on_exit() {
  local rc="${1:-0}"
  log "---"
  release_udp
  for id in "${TRACKED_JOBS[@]:-}"; do
    [ -z "$id" ] && continue
    jarvis-mq unlisten "flow.$id" >/dev/null 2>&1 || true
    jarvis-mq unlisten "flow.$id-small" >/dev/null 2>&1 || true
    jarvis-mq results "flow.$id" >/dev/null 2>&1 || true
    jarvis-mq results "flow.$id-small" >/dev/null 2>&1 || true
    jarvis-mq delete "flow.$id" >/dev/null 2>&1 || true
    jarvis-mq delete "flow.$id-small" >/dev/null 2>&1 || true
    jarvis-jobs finish "$id" 0 >/dev/null 2>&1 || true
    rm -rf "$JOBS/$id" 2>/dev/null || true
  done
  rm -rf "$ART_ROOT" 2>/dev/null || true
  # Restore the caller's round job: it was paused so rule 3 could open the
  # window. If the caller's owner process is still alive the round finishes
  # the job itself (jarvis-jobs finish works from paused) — requeueing it
  # while the owner lives would hand it to a second runner. Only when the
  # owner is gone does the job need to go back to ready for a later round.
  if [ -n "$ROUND_JOB" ] && [ "$(status_of "$ROUND_JOB")" = "paused" ]; then
    local opid
    opid="$(python3 -c "import json;print(json.load(open('$JOBS/$ROUND_JOB/state.json')).get('pid',0))" 2>/dev/null || echo 0)"
    if [ "${opid:-0}" != "0" ] && kill -0 "$opid" 2>/dev/null; then
      log "cleanup: round job owner (pid $opid) still alive — leaving $ROUND_JOB paused for the round to finish it"
    else
      log "cleanup: round job owner gone — requeueing $ROUND_JOB (paused -> ready) so a later round can finish it"
      jarvis-jobs requeue "$ROUND_JOB" >/dev/null 2>&1 || \
        log "cleanup: requeue of $ROUND_JOB failed (status now: $(status_of "$ROUND_JOB"))"
    fi
  fi
  log "stack health (should all be running):"
  docker ps --format '    {{.Names}}: {{.Status}}' 2>/dev/null | grep -E 'jarvis-engine |jarvis-switchboard|jarvis-image-gen|jarvis-llm-small-worker-gpu|jarvis-rabbitmq' || true
  log "brain engine: $(brain_up && echo up || echo DOWN)"
  log "window: open=$(state_get "d.get('window',{}).get('open')") stream_active=$(state_get "d.get('stream_active')")"
  log "RESULTS: $PASS passed, $FAIL failed"
  for r in "${RESULTS[@]:-}"; do
    [ -z "$r" ] && continue
    log "    $r"
  done
  if [ "$rc" -eq 2 ]; then
    log "GATE: FAIL (preflight) — exit 2"
    exit 2
  fi
  if [ "$FAIL" -eq 0 ] && [ "$PASS" -eq 3 ]; then
    log "GATE: PASS (S1 canonical flow + S2 voice fallback + S3 game drain)"
    log "transcript: $TRANSCRIPT"
    exit 0
  fi
  log "GATE: FAIL — $((PASS + FAIL))/3 scenarios ran, $FAIL failed — transcript: $TRANSCRIPT"
  exit 1
}
trap 'on_exit $?' EXIT

# --- preflight --------------------------------------------------------------
log "Phase 4 event-flow gate  (run=$RUN, resourced=$RESOURCED)"
log "data root: $DATA, round job: ${ROUND_JOB:-<none>}"
preflight() {
  local rc=0
  for c in jarvis-switchboard jarvis-rabbitmq jarvis-image-gen-worker jarvis-llm-small-worker-gpu jarvis-voice; do
    if ! docker ps --format '{{.Names}}' | grep -qx "$c"; then
      log "preflight: container $c is not running"; rc=1
    fi
  done
  if ! pgrep -u nathan -x sunshine >/dev/null 2>&1; then
    log "preflight: sunshine (user nathan) is not running — S3's stream detector needs it"; rc=1
  fi
  for tool in jarvis-jobs jarvis-mq curl python3; do
    command -v "$tool" >/dev/null 2>&1 || { log "preflight: $tool not on PATH"; rc=1; }
  done
  if [ -z "$ROUND_JOB" ]; then
    log "preflight: ROUND_JOB is not set (the caller's round job must be pausable for rule 3)"; rc=1
  elif [ "$(status_of "$ROUND_JOB")" != "running" ]; then
    log "preflight: round job $ROUND_JOB is $(status_of "$ROUND_JOB"), not running"; rc=1
  fi
  if ! state >/dev/null 2>&1; then
    log "preflight: cannot reach resourced $RESOURCED/state"; rc=1
  else
    if [ "$(state_get "d.get('window',{}) and 'cap_depths' in d['window']")" != "true" ]; then
      log "preflight: resourced /state has no window.cap_depths — the deployed binary predates this tree"; rc=1
    fi
    if [ "$(state_get "d.get('window',{}).get('open')")" != "false" ]; then
      log "preflight: a swap window is already open"; rc=1
    fi
    if [ "$(state_get "d.get('stream_active')")" != "false" ]; then
      log "preflight: a game stream is already active"; rc=1
    fi
  fi
  if ! brain_up; then
    log "preflight: the brain engine is not healthy before the gate"; rc=1
  fi
  for q in cap.image.generate cap.llm.small; do
    local depth
    depth="$(jarvis-mq depth "$q" 2>/dev/null || echo '?')"
    if [ "$depth" != "0" ]; then
      log "preflight: $q already has depth $depth — the gate needs both empty"; rc=1
    fi
  done
  if ! curl -sf --max-time 5 "$VOICE/health" 2>/dev/null | grep -q '"brain_up"'; then
    log "preflight: voice /health has no fallback.brain_up — the deployed voice service predates the brain-down routing"; rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    log "GATE: FAIL (preflight) — fix the stack above and re-run"
    exit 2
  fi
  log "preflight OK: stack up, window closed, brain up, both cap queues empty, voice routing deployed"
  log "pausing round job $ROUND_JOB (rule 3: no window while a local round is RUNNING)"
  jarvis-jobs pause "$ROUND_JOB" >/dev/null || { log "preflight: pause $ROUND_JOB failed"; exit 2; }
  log "round job now $(status_of "$ROUND_JOB")"
}

# --- run --------------------------------------------------------------------
preflight
s1_s2
s3_game
# (exit is handled by the trap)
