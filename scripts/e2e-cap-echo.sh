#!/usr/bin/env bash
#
# e2e-cap-echo.sh — Phase 2 wait/wake gate (event-driven brain plan, task 17).
#
# Proves the whole event loop end-to-end on the live box, with a scripted
# stand-in round (jarvis-jobs mark-running + yield) instead of a real brain
# round — the plan explicitly sanctions a stand-in: a job is submitted to the
# cap.echo test worker, the round yields (running -> waiting), the worker
# replies after its delay, and the switchboard wakes the job (waiting -> ready)
# on the result. Then the two resilience cases (kill the switchboard mid-wait,
# restart the broker mid-wait) and the timeout path (deadline fires with no
# result -> status=timeout).
#
# Scenarios:
#   S1  main flow          submit -> yield -> echo replies -> wake within 5 s
#   S2  switchboard down   kill switchboard mid-wait; result lands while down;
#                          restart switchboard -> still wakes (durable result +
#                          durable listener survive)
#   S3  broker restart     restart rabbitmq mid-wait; the durable cap message is
#                          re-consumed after reconnect -> still wakes
#   S4  timeout            submit to a capability with NO worker (sanctioned);
#                          the wait deadline fires -> wake with status=timeout
#
# Every scenario uses fresh gate-<epoch>-sN job ids / flow queues / artifact
# dirs under the data root (the only path the cap-echo container shares with
# the host), and cleans them up. A trap restores the switchboard's restart
# policy and verifies the stack is healthy on the way out.
#
# Usage:  bash scripts/e2e-cap-echo.sh [transcript.log]
#         (exit 0 = all scenarios pass; 1 = any failed; 2 = preflight failed)

set -uo pipefail

# --- config -----------------------------------------------------------------
DATA="${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}"
JOBS="$DATA/jobs"
LISTENERS="$DATA/listeners"
# Under the cap-echo container's shared mount so the host can verify the file
# the container wrote.
ART_ROOT="$DATA/artifacts/cap-echo-gate"
RUN="$(date -u +%s)"
ECHO_DELAY_S=30                       # matches the cap-echo compose default
WAKE_BUDGET_MS=75000                  # S1/S2: allow the 30 s echo + margin
S3_BUDGET_MS=90000                    # S3: broker restart + reconnect + 30 s
TIMEOUT_WAIT_UNTIL=6s                 # S4: short wait deadline
TIMEOUT_BUDGET_MS=30000

TRANSCRIPT="${1:-/tmp/e2e-cap-echo-$RUN.log}"
mkdir -p "$ART_ROOT"
: > "$TRANSCRIPT"
exec > "$TRANSCRIPT" 2>&1

PASS=0
FAIL=0
declare -a RESULTS=()     # "<name>:PASS|FAIL"
declare -a TRACKED_JOBS=()

# --- helpers ----------------------------------------------------------------
# Millisecond clock. `date +%s%3N` is not portable (some builds emit an
# 18-digit seconds+nanos blob), so compute it from a clean %s%N instead.
# The 10# forces base 10: a nanosecond field starting with 0 would otherwise
# be read as octal and blow up on the first 8/9 digit (~10% of samples).
now_ms() {
  local t; t="$(date +%s%N)"
  echo $(( 10#${t:0:10} * 1000 + 10#${t:10:9} / 1000000 ))
}
log()  { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# state.json / wake.json are pretty-printed ("status": "waiting" — space after
# the colon), so match with a flexible pattern and pull the value with sed.
status_of() { # status_of <jobid> -> the state.json status (empty if gone)
  grep -oE '"status"[[:space:]]*:[[:space:]]*"[a-z]+"' "$JOBS/$1/state.json" 2>/dev/null \
    | head -1 | sed -E 's/.*:[[:space:]]*"([a-z]+)"/\1/'
}
wake_reason_of() { # wake_reason_of <jobid> -> wake.json reason (empty if none)
  grep -oE '"reason"[[:space:]]*:[[:space:]]*"[a-z]+"' "$JOBS/$1/wake.json" 2>/dev/null \
    | head -1 | sed -E 's/.*:[[:space:]]*"([a-z]+)"/\1/'
}

track() { TRACKED_JOBS+=("$1"); }

# finish_and_rm <jobid> — finish the stand-in job (best effort) and remove its
# fixture dir so it never lingers in the live store.
finish_and_rm() {
  local id="$1"
  jarvis-jobs finish "$id" 0 >/dev/null 2>&1 || true
  rm -rf "$JOBS/$id" 2>/dev/null || true
}

# cleanup_scenario <jobid> <flow> — best-effort teardown of one scenario's
# fixtures (called from the FAIL paths; the success paths clean inline).
cleanup_scenario() {
  local id="$1" flow="$2"
  jarvis-mq unlisten "$flow" >/dev/null 2>&1 || true
  jarvis-mq results "$flow" >/dev/null 2>&1 || true   # drain any loose result
  jarvis-mq delete "$flow" >/dev/null 2>&1 || true
  finish_and_rm "$id"
}

fail() { # fail <name> <reason>
  log "FAIL $1: $2"
  RESULTS+=("$1:FAIL")
  FAIL=$((FAIL + 1))
}
ok() { # ok <name>
  log "PASS $1"
  RESULTS+=("$1:PASS")
  PASS=$((PASS + 1))
}

# wait_for_wake <jobid> <artifact> <budget_ms> — poll until the job is ready
# (woken) or the budget expires. Sets T_WAKE (ms) and T_ART (ms, best effort).
wait_for_wake() {
  local id="$1" art="$2" budget="$3"
  local t0 t_art="" t_wake="" st
  t0="$(now_ms)"
  T_WAKE=""; T_ART=""
  while :; do
    [ -z "$t_art" ] && [ -f "$art" ] && t_art="$(now_ms)"
    st="$(status_of "$id")"
    if [ "$st" = "ready" ]; then t_wake="$(now_ms)"; break; fi
    if [ $(( $(now_ms) - t0 )) -gt "$budget" ]; then return 1; fi
    sleep 0.25
  done
  T_WAKE="$t_wake"; T_ART="$t_art"
  return 0
}

# wait_file <path> <budget_ms> — poll until the file exists.
wait_file() {
  local f="$1" budget="$2" t0
  t0="$(now_ms)"
  while [ ! -f "$f" ]; do
    if [ $(( $(now_ms) - t0 )) -gt "$budget" ]; then return 1; fi
    sleep 0.5
  done
  return 0
}

# wait_broker_up <budget_s> — poll until the broker answers.
wait_broker_up() {
  local budget_s="$1" t0
  t0="$(now_ms)"
  while ! jarvis-mq depth cap.echo >/dev/null 2>&1; do
    if [ $(( $(now_ms) - t0 )) -gt $(( budget_s * 1000 )) ]; then return 1; fi
    sleep 1
  done
  return 0
}

# verify_common <name> <id> <flow> <corr> <outdir> [want_reason] — after a
# wake: check the wake reason, drain + inspect the Result, and confirm the
# artifact the worker wrote exists and is referenced.
verify_common() {
  local name="$1" id="$2" flow="$3" corr="$4" out="$5" want_reason="${6:-all}"
  local reason res
  reason="$(wake_reason_of "$id")"
  if [ -n "$want_reason" ] && [ "$reason" != "$want_reason" ]; then
    fail "$name" "wake reason=$reason, want $want_reason"
    cleanup_scenario "$id" "$flow"
    return 1
  fi
  res="$(jarvis-mq results "$flow" 2>&1)"
  if [ "$want_reason" = "all" ] && ! grep -q "status=ok" <<<"$res"; then
    fail "$name" "Result on $flow is not status=ok: $res"
    cleanup_scenario "$id" "$flow"
    return 1
  fi
  if [ "$want_reason" = "all" ]; then
    local art="$out/echo-$corr.txt"
    if [ ! -s "$art" ]; then
      fail "$name" "artifact missing/empty: $art"
      cleanup_scenario "$id" "$flow"
      return 1
    fi
    log "$name: referenced artifact $(head -1 "$art")"
  fi
  jarvis-mq unlisten "$flow" >/dev/null 2>&1 || true
  jarvis-mq delete "$flow" >/dev/null 2>&1 || true
  finish_and_rm "$id"
  return 0
}

# setup <id> <flow> <corr> <outdir> <capability> <wait_until> — the shared
# stand-in flow: add -> mark-running -> submit -> listen -> yield. Leaves the
# job waiting.
setup() {
  local id="$1" flow="$2" corr="$3" out="$4" cap="$5" wait_until="${6:-}"
  track "$id"
  local wy=()
  [ -n "$wait_until" ] && wy=(--wait-until "$wait_until")
  jarvis-jobs add -id "$id" -title "Phase 2 gate: $cap" -driver local \
    -instructions "stand-in round for the cap-echo gate" >/dev/null \
    || { log "add $id failed"; return 1; }
  jarvis-jobs mark-running "$id" >/dev/null || { log "mark-running $id failed"; return 1; }
  jarvis-mq submit "$cap" --reply-to "$flow" --corr "$corr" --out-dir "$out" \
    --wake-job "$id" >/dev/null || { log "submit $cap failed"; return 1; }
  jarvis-mq listen "$flow" --wake "$id" --corr "$corr" >/dev/null \
    || { log "listen $flow failed"; return 1; }
  jarvis-jobs yield "$id" --wait-on "$corr" --note "waiting for $cap" \
    --in-flight "- $corr: $cap request" \
    --next "- read $flow result, verify artifact, finish" \
    "${wy[@]}" >/dev/null || { log "yield $id failed"; return 1; }
  return 0
}

# --- scenarios --------------------------------------------------------------
s1_main_flow() {
  local id="gate-$RUN-s1"
  local flow="flow.$id" corr="s1-$RUN" out="$ART_ROOT/$id"
  log "S1: main flow — submit cap.echo, yield, expect a result wake < 5 s"
  if ! setup "$id" "$flow" "$corr" "$out" "echo"; then fail S1 "setup"; return; fi
  log "S1: $id is now $(status_of "$id"); waiting for the echo reply + wake (budget $((WAKE_BUDGET_MS / 1000)) s)"
  if ! wait_for_wake "$id" "$out/echo-$corr.txt" "$WAKE_BUDGET_MS"; then
    fail S1 "no wake within $((WAKE_BUDGET_MS / 1000)) s (status=$(status_of "$id"))"; cleanup_scenario "$id" "$flow"; return
  fi
  local lat="n/a"
  if [ -n "$T_ART" ] && [ -n "$T_WAKE" ]; then lat="$(( T_WAKE - T_ART ))"; fi
  log "S1: wake reason=$(wake_reason_of "$id"), artifact->wake latency=${lat}ms (bar: < 5000ms)"
  if [ -n "$T_ART" ] && [ $(( T_WAKE - T_ART )) -gt 5000 ]; then
    fail S1 "wake latency ${lat}ms exceeds the 5 s bar"; cleanup_scenario "$id" "$flow"; return
  fi
  verify_common S1 "$id" "$flow" "$corr" "$out" "all" && ok S1
}

s2_switchboard_down() {
  local id="gate-$RUN-s2"
  local flow="flow.$id" corr="s2-$RUN" out="$ART_ROOT/$id"
  log "S2: kill the switchboard mid-wait; the result must land while it is down, then wake on restart"
  if ! setup "$id" "$flow" "$corr" "$out" "echo"; then fail S2 "setup"; return; fi
  log "S2: disabling auto-restart and killing the switchboard (mid-wait)"
  docker update --restart=no jarvis-switchboard >/dev/null 2>&1 || true
  docker kill jarvis-switchboard >/dev/null 2>&1 || true
  if ! wait_file "$out/echo-$corr.txt" "$WAKE_BUDGET_MS"; then
    fail S2 "echo produced no result within budget while the switchboard was down"; cleanup_scenario "$id" "$flow"; return
  fi
  log "S2: result landed on $flow while the switchboard was down; restarting it"
  docker start jarvis-switchboard >/dev/null 2>&1 || true
  if ! wait_for_wake "$id" "$out/echo-$corr.txt" 30000; then
    fail S2 "no wake after switchboard restart (status=$(status_of "$id"))"; cleanup_scenario "$id" "$flow"; return
  fi
  log "S2: woken after restart (reason=$(wake_reason_of "$id"))"
  verify_common S2 "$id" "$flow" "$corr" "$out" "all" && ok S2
}

s3_broker_restart() {
  local id="gate-$RUN-s3"
  local flow="flow.$id" corr="s3-$RUN" out="$ART_ROOT/$id"
  log "S3: restart the broker mid-wait; the durable cap message must be re-consumed and still wake"
  if ! setup "$id" "$flow" "$corr" "$out" "echo"; then fail S3 "setup"; return; fi
  log "S3: restarting jarvis-rabbitmq (the most disruptive case — other services reconnect)"
  docker restart jarvis-rabbitmq >/dev/null 2>&1 || true
  if ! wait_broker_up 60; then fail S3 "broker did not come back within 60 s"; cleanup_scenario "$id" "$flow"; return; fi
  log "S3: broker is back; waiting for the echo result (re-consume + 30 s; budget $((S3_BUDGET_MS / 1000)) s)"
  if ! wait_file "$out/echo-$corr.txt" "$S3_BUDGET_MS"; then
    fail S3 "no echo result within $((S3_BUDGET_MS / 1000)) s after the broker restart"; cleanup_scenario "$id" "$flow"; return
  fi
  if ! wait_for_wake "$id" "$out/echo-$corr.txt" 30000; then
    fail S3 "no wake after broker restart (status=$(status_of "$id"))"; cleanup_scenario "$id" "$flow"; return
  fi
  log "S3: woken after broker restart (reason=$(wake_reason_of "$id"))"
  verify_common S3 "$id" "$flow" "$corr" "$out" "all" && ok S3
}

s4_timeout() {
  local id="gate-$RUN-s4"
  local flow="flow.$id" corr="s4-$RUN" out="$ART_ROOT/$id"
  log "S4: submit to a capability with NO worker; the wait deadline must fire -> status=timeout"
  if ! setup "$id" "$flow" "$corr" "$out" "gate.timeout" "$TIMEOUT_WAIT_UNTIL"; then fail S4 "setup"; return; fi
  log "S4: $id is waiting (wait-until $TIMEOUT_WAIT_UNTIL); the switchboard checks deadlines every 5 s (budget $((TIMEOUT_BUDGET_MS / 1000)) s)"
  local t0
  t0="$(now_ms)"
  while [ "$(status_of "$id")" != "ready" ]; do
    if [ $(( $(now_ms) - t0 )) -gt "$TIMEOUT_BUDGET_MS" ]; then
      fail S4 "no timeout wake within $((TIMEOUT_BUDGET_MS / 1000)) s (status=$(status_of "$id"))"; cleanup_scenario "$id" "$flow"; return
    fi
    sleep 0.5
  done
  log "S4: woken (reason=$(wake_reason_of "$id")); removing the dead cap.gate.timeout queue"
  docker exec -i jarvis-rabbitmq rabbitmqctl delete_queue cap.gate.timeout >/dev/null 2>&1 || true
  verify_common S4 "$id" "$flow" "$corr" "$out" "timeout" && ok S4
}

# --- cleanup / exit trap ----------------------------------------------------
on_exit() {
  log "---"
  log "cleanup: restoring switchboard (restart policy + a stop is not undone by policy, so start it if down)"
  docker update --restart=always jarvis-switchboard >/dev/null 2>&1 || true
  docker ps --format '{{.Names}}' 2>/dev/null | grep -qx jarvis-switchboard \
    || docker start jarvis-switchboard >/dev/null 2>&1 || true
  # Safety sweep: finish + remove any tracked job that a scenario left behind,
  # and clear any listener on its flow queue.
  for id in "${TRACKED_JOBS[@]:-}"; do
    [ -z "$id" ] && continue
    jarvis-mq unlisten "flow.$id" >/dev/null 2>&1 || true
    finish_and_rm "$id"
  done
  # Remove the S4 dead cap queue if it was not already gone.
  docker exec -i jarvis-rabbitmq rabbitmqctl delete_queue cap.gate.timeout >/dev/null 2>&1 || true
  log "stack health (should all be running):"
  docker ps --format '    {{.Names}}: {{.Status}}' 2>/dev/null | grep -E 'jarvis-switchboard|jarvis-cap-echo|jarvis-rabbitmq' || true
  log "RESULTS: $PASS passed, $FAIL failed"
  for r in "${RESULTS[@]:-}"; do
    [ -z "$r" ] && continue
    log "    $r"
  done
  local ran=$(( PASS + FAIL ))
  if [ "$FAIL" -eq 0 ] && [ "$ran" -eq 4 ]; then
    log "GATE: PASS (all 4 scenarios)"
    log "transcript: $TRANSCRIPT"
    exit 0
  fi
  log "GATE: FAIL — $ran/4 scenarios ran, $FAIL failed — transcript: $TRANSCRIPT"
  exit 1
}
trap on_exit EXIT

# --- preflight --------------------------------------------------------------
log "Phase 2 wait/wake gate  (run=$RUN, echo delay=${ECHO_DELAY_S}s)"
log "data root: $DATA"
preflight() {
  local rc=0
  for c in jarvis-switchboard jarvis-cap-echo jarvis-rabbitmq; do
    if ! docker ps --format '{{.Names}}' | grep -qx "$c"; then
      log "preflight: container $c is not running"; rc=1
    fi
  done
  if ! command -v jarvis-jobs >/dev/null 2>&1 || ! command -v jarvis-mq >/dev/null 2>&1; then
    log "preflight: jarvis-jobs / jarvis-mq not on PATH"; rc=1
  fi
  if ! jarvis-mq depth cap.echo >/dev/null 2>&1; then
    log "preflight: cannot reach the broker / cap.echo"; rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    log "GATE: FAIL (preflight) — fix the stack above and re-run"
    exit 2
  fi
  log "preflight OK: switchboard + cap-echo + rabbitmq up, cap.echo reachable"
}

# --- run --------------------------------------------------------------------
preflight
s1_main_flow
s2_switchboard_down
s3_broker_restart
s4_timeout
# (exit is handled by the trap)
