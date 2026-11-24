#!/usr/bin/env bash
# moe-128k-point.sh — one ladder point of the MoE 128K context suite.
#
# Plan: docs/benchmarks/moe-128k-suite-plan.md. The suite fixes the context
# at 128K (131072 max seq len, KV reserved for 128K tokens) and sweeps the
# free-VRAM ladder 400/300/250/200/150/100 MB per model. This script does
# one point end to end:
#
#   1. open a watchdog maintenance window (protects the stopped brain),
#   2. stop the brain (compose `engine`), verify VRAM freed,
#   3. start `ft serve` on :11436 with --memory-ratio calibrated to the
#      target free VRAM (start estimate + at most one correction),
#   4. record the resolved moe_cache_size / num_pages / free VRAM,
#   5. run the three suite runners (standard sweep, coding battery,
#      128K needle) from bench/coding/,
#   6. write the point's results doc (docs/benchmarks/ + bench dir copy),
#   7. kill ft by PID (orphan workers included), restore the brain and
#      verify both engines (:11434 + :11435),
#   8. print one SUMMARY line for the round's agent.
#
# Usage:  scripts/moe-128k-point.sh <model-slug> <target-free-MB>
#   model-slug  qwen3.8-flash-next | glm5.3-flash
#   target      400 | 300 | 250 | 200 | 150 | 100
#
# Exit 0 = point complete. Component-level failures (a failed exercise, a
# wrong needle position, an errored standard run) are RESULTS recorded in
# the JSONs and the doc, not point failures.
# Exit 1 = point failed: the engine could not serve, a runner could not
# reach the endpoint, or the brain could not be restored. The reason is on
# the SUMMARY line and in <out>/point.log.
#
# The brain stop/restore is trapped: every exit path kills ft by PID first
# and re-arms the brain, so a dead round cannot strand the 4090 (the
# watchdog maintenance window is the backstop if even this process is
# killed hard — the 2h TTL expires and brain checks resume).
#
# Safe to run only when nothing else needs the 4090. Never run two points
# at once. The iGPU helper (:11435) is never touched by this script.
set -u

SLUG="${1:?usage: scripts/moe-128k-point.sh <model-slug> <target-free-MB>}"
TARGET_MB="${2:?usage: scripts/moe-128k-point.sh <model-slug> <target-free-MB>}"
case "$TARGET_MB" in 400|300|250|200|150|100) ;;
  *) echo "unknown ladder target: $TARGET_MB (want 400|300|250|200|150|100)" >&2; exit 2 ;;
esac

REPO=/home/agent/ai-agent-build
case "$SLUG" in
  qwen3.8-flash-next) QDIR=/home/agent/models/qwen3.8-flash-next/UD-IQ4_XS
                      QUANT=UD-IQ4_XS ;;
  glm5.3-flash)       QDIR=/home/agent/models/glm5.3-flash/UD-IQ2_XXS
                      QUANT=UD-IQ2_XXS ;;
  *) echo "unknown model slug: $SLUG (want qwen3.8-flash-next | glm5.3-flash)" >&2; exit 2 ;;
esac
SHARD="$(ls "$QDIR"/*.gguf 2>/dev/null | sort | head -1)"
[[ -n "$SHARD" && -f "$SHARD" ]] || { echo "first shard missing in $QDIR" >&2; exit 2; }

FT=/home/agent/ftvenv/bin/ft
OUT="$HOME/.local/share/jarvis/bench/moe-128k/${SLUG}-${TARGET_MB}"
DOC="$REPO/docs/benchmarks/moe-128k-${SLUG}-${TARGET_MB}mb.md"
FTLOG=$OUT/ft-serve.log
PIDF=$OUT/ft.pid
BASE=http://127.0.0.1:11436/v1
COMPOSE="docker compose -f $REPO/docker-compose.yml"
mkdir -p "$OUT"

POINT_RC=0        # 0 until proven otherwise; cleanup reads it
POINT_FAIL=""     # reason for a non-zero exit
FT_PID=""
echo $$ > "$OUT/point.pid"   # the launcher and checker track this pid
R_USED=""         # memory-ratio actually used for the completed start
F_FREE=""         # free VRAM (MiB) measured after load, for the completed start
CALIB_NOTE="no correction needed"

log() { echo "[point] $*" | tee -a "$OUT/point.log"; }
fail() { POINT_RC=1; POINT_FAIL="$1"; log "FAILED: $1"; }

free_mib() { nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | head -1 | tr -d ' '; }

# ---------------------------------------------------------------- brain --
stop_brain() {
  log "stopping the brain (compose engine)"
  $COMPOSE stop engine >/dev/null 2>&1 || true
  local f i
  for i in $(seq 1 60); do
    f=$(free_mib); (( ${f:-0} > 23000 )) && { log "VRAM freed: ${f} MiB free"; return 0; }
    sleep 2
  done
  return 1
}

restore_verify() {
  $COMPOSE start engine >/dev/null 2>&1 || true
  local i ok=1
  for i in $(seq 1 90); do
    curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1 && break
    sleep 5
  done
  curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1 || ok=0
  curl -sf http://127.0.0.1:11435/v1/models >/dev/null 2>&1 || ok=0
  if (( ok )); then log "brain :11434 + iGPU helper :11435 verified up"; else
    log "ENGINE RESTORE FAILED (brain or iGPU not answering)"; return 1
  fi
  return 0
}

kill_ft() {
  [[ -n "$FT_PID" ]] || return 0
  log "killing ft serve (pid $FT_PID)"
  kill "$FT_PID" 2>/dev/null
  local i
  for i in $(seq 1 60); do kill -0 "$FT_PID" 2>/dev/null || break; sleep 2; done
  kill -0 "$FT_PID" 2>/dev/null && kill -9 "$FT_PID" 2>/dev/null
  FT_PID=""
  sleep 5
  # Orphan spawn_main workers survive the API server's exit and hold
  # ~21 GB (the VRAM sweeps learned it the hard way). Kill them by PID.
  local pids p
  pids="$(nvidia-smi --query-compute-apps=pid --format=csv,noheader 2>/dev/null | tr -d ' ' | grep -v '^$' || true)"
  if [[ -n "$pids" ]]; then
    log "killing orphan GPU workers: $(echo "$pids" | tr '\n' ' ')"
    for p in $pids; do kill "$p" 2>/dev/null || true; done
    sleep 10
  fi
}

cleanup() {
  local rc=$?
  if (( rc != 0 )); then
    POINT_RC=1
    [[ -n "$POINT_FAIL" ]] || POINT_FAIL="point script exited $rc"
    log "POINT FAILED (exit $rc: $POINT_FAIL) — restoring the brain"
  fi
  kill_ft
  if ! restore_verify; then
    POINT_RC=1
    POINT_FAIL="${POINT_FAIL:+$POINT_FAIL; }brain/iGPU not verified up after restore"
  fi
  jarvis-watchdog maintain off >/dev/null 2>&1 || true
  local s="FAILED ${POINT_FAIL}"
  (( POINT_RC == 0 )) && s="OK r=$R_USED free=${F_FREE}MiB $CALIB_NOTE"
  echo "SUMMARY moe-128k $SLUG ${TARGET_MB}mb: $s" | tee -a "$OUT/point.log"
  # Fire the follow-up jobs from HERE, not from a model round: the launcher
  # and checker rounds can die (iGPU OOM / context), but this trap always
  # runs. OK -> the checker verifies; FAILED -> the sonnet fallback re-runs.
  if [[ -n "${MOE_DONE_HOOK:-}" && -x "${MOE_DONE_HOOK}" ]]; then
    "$MOE_DONE_HOOK" "$SLUG" "$TARGET_MB" "$POINT_RC" >>"$OUT/hook.log" 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT
# A hard kill from the launcher's timeout cap must still run cleanup (kill ft,
# restore the brain). TERM exits through the EXIT trap.
trap 'POINT_FAIL="killed by signal (timeout cap?)"; exit 124' TERM INT

# ------------------------------------------------------------ pre-flight --
# Preflight failures go to point.log as well as stderr: a detached point has
# no terminal, and the 11-22 runs lost their preflight stderr with the dead
# round.
pfail() { echo "PREFLIGHT $*" >&2; echo "PREFLIGHT $*" >> "$OUT/point.log"; exit 2; }
[[ -x "$FT" ]] || pfail "ft not found at $FT"
command -v ninja >/dev/null 2>&1 || PATH=/home/agent/ftvenv/bin:$PATH
[[ -f "$REPO/bench/coding/run_standard.py" && -f "$REPO/docs/benchmarks/moe-sweep-prompt.txt" ]] \
  || { echo "harness or prompt file missing in $REPO" >&2; exit 2; }
# The brain must be up (this point stops it). Retry 10 x 60 s: a photo swap
# window or a transient engine blip at launch must not kill the point (the
# 11-22 points all died on one failed curl).
i=0
until curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1; do
  i=$((i + 1))
  (( i >= 10 )) && pfail "brain (:11434) not up after 10 min — refusing to start"
  log "preflight: brain (:11434) not answering yet (try $i/10), waiting 60 s"
  sleep 60
done
curl -sf http://127.0.0.1:11436/v1/models >/dev/null 2>&1 \
  && pfail ":11436 already answers — another ft serve is running"
# The window must outlive the launcher's 3 h cap + 15 min cleanup grace.
jarvis-watchdog maintain 4h "moe-128k point $SLUG ${TARGET_MB}mb" >/dev/null 2>&1 \
  || log "warning: could not open a watchdog maintenance window"

# ------------------------------------------------------------------ run --
stop_brain || { fail "VRAM not freed after stopping the brain (still $(free_mib) MiB free)"; exit 1; }

R0=$(python3 -c "print(round(1 - $TARGET_MB/20000.0, 4))")

start_ft() {  # $1 = memory-ratio; sets FT_PID; 0 = ready
  local r="$1" i
  log "starting ft serve (memory-ratio=$r)"
  PATH=/home/agent/ftvenv/bin:$PATH "$FT" serve \
    --model "$SHARD" --port 11436 --moe-strategy hybrid --moe-cache-auto \
    --max-seq-len-override 131072 --kv-reserve-tokens 131072 \
    --memory-ratio "$r" >"$FTLOG" 2>&1 &
  FT_PID=$!
  echo "$FT_PID" > "$PIDF"
  for i in $(seq 1 240); do
    grep -q "API server is ready to serve" "$FTLOG" 2>/dev/null && return 0
    kill -0 "$FT_PID" 2>/dev/null || { log "ft serve died during start (see $FTLOG)"; return 1; }
    sleep 5
  done
  log "ft serve did not become ready within 20 min (see $FTLOG)"
  return 1
}

start_ft "$R0" || { fail "ft serve (ratio $R0) did not come up"; exit 1; }

# Calibrate: free VRAM is ~linear in (1 - ratio); one correction if the
# start landed more than 40 MB off the target.
F_FREE=$(free_mib)
if [[ -n "${F_FREE:-}" ]]; then
  DIFF=$(( F_FREE - TARGET_MB )); (( DIFF < 0 )) && DIFF=$(( -DIFF ))
  if (( DIFF > 40 )); then
    R1=$(python3 -c "
f, r0, t = $F_FREE, $R0, $TARGET_MB
s = f / (1 - r0)          # MiB per unit ratio
r1 = 1 - t / s
print(round(r1, 4) if 0.90 < r1 < 0.9995 else '')")
    if [[ -n "$R1" ]]; then
      log "free ${F_FREE} MiB is ${DIFF} off target ${TARGET_MB} — correcting ratio $R0 -> $R1"
      kill_ft
      start_ft "$R1" || { fail "ft serve (corrected ratio $R1) did not come up"; exit 1; }
      F_FREE=$(free_mib)
      CALIB_NOTE="corrected $R0 -> $R1"
      DIFF=$(( F_FREE - TARGET_MB )); (( DIFF < 0 )) && DIFF=$(( -DIFF ))
      (( DIFF > 40 )) && CALIB_NOTE="$CALIB_NOTE (still ${DIFF} MB off — recorded as-is)"
    else
      CALIB_NOTE="correction out of range — ran at $R0"
    fi
  fi
fi
R_USED="${R1:-$R0}"
grep -q "API server is ready to serve" "$FTLOG" 2>/dev/null \
  || { fail "endpoint not ready after calibration"; exit 1; }

# The resolved values are the dependent variable of this suite — record the
# engine's own lines verbatim (moe_cache_size is in slots; see the plan).
grep -a "moe-cache-auto resolved\|Free GPU memory after capturing\|GGUF expert banks\|Resolved config" \
  "$FTLOG" > "$OUT/engine-lines.txt" 2>/dev/null || true

# ------------------------------------------------------------ the suite --
log "running the suite (standard -> exercises -> needle) against $BASE"
ST_OUT=$OUT/standard.out; EX_OUT=$OUT/exercises.out; ND_OUT=$OUT/needle.out
cd "$REPO"

python3 bench/coding/run_standard.py --base-url "$BASE" \
  --prompt docs/benchmarks/moe-sweep-prompt.txt --out "$OUT/standard.json" > "$ST_OUT" 2>&1 \
  || log "run_standard rc=$? (see $ST_OUT)"
python3 bench/coding/run_exercises.py --base-url "$BASE" --out "$OUT/exercises.json" > "$EX_OUT" 2>&1 \
  || log "run_exercises rc=$? (see $EX_OUT)"
python3 bench/coding/needle_128k.py --base-url "$BASE" --out "$OUT/needle.json" > "$ND_OUT" 2>&1 \
  || log "needle_128k rc=$? (see $ND_OUT)"

# Endpoint-down detection (the runners record errors as results, so the
# JSONs decide): a point fails only if the engine died or a component could
# not run at all.
kill -0 "$FT_PID" 2>/dev/null || fail "ft serve died during the suite"
curl -sf "$BASE/models" >/dev/null 2>&1 || fail "endpoint :11436 unreachable after the suite"
python3 - "$OUT" <<'EOF' || fail "a suite component could not reach the endpoint (see point.log)"
import json, sys, os
out = sys.argv[1]
def bad(name, cond):
    if cond:
        print(f"component {name} produced no usable runs", file=sys.stderr)
        return True
    return False
ok = True
try:
    std = json.load(open(os.path.join(out, "standard.json")))
    ok &= not bad("standard", all("error" in r for r in std.get("runs", [])) or not std.get("runs"))
except Exception as e:
    ok = False; print(f"standard.json unreadable: {e}", file=sys.stderr)
for name in ("exercises", "needle"):
    try:
        d = json.load(open(os.path.join(out, name + ".json")))
        ok &= not bad(name, not d.get("total"))
    except Exception as e:
        ok = False; print(f"{name}.json unreadable: {e}", file=sys.stderr)
sys.exit(0 if ok else 1)
EOF

# ------------------------------------------------------- results doc --
python3 - "$OUT" "$DOC" "$SLUG" "$TARGET_MB" "$QUANT" "$R_USED" "$F_FREE" "$CALIB_NOTE" <<'EOF'
import json, os, sys, time
out, doc, slug, target, quant, r, free, calib = sys.argv[1:9]
def load(n):
    try: return json.load(open(os.path.join(out, n)))
    except Exception: return {}
std, ex, nd = load("standard.json"), load("exercises.json"), load("needle.json")
eng = open(os.path.join(out, "engine-lines.txt")).read().strip() if os.path.exists(os.path.join(out, "engine-lines.txt")) else "(no engine lines captured)"
lines = []
a = lines.append
a(f"# MoE 128K suite — {slug} ({quant}), {target} MB free-VRAM point")
a("")
a(f"Run {time.strftime('%Y-%m-%d %H:%M UTC', time.gmtime())} by")
a("`scripts/moe-128k-point.sh` (plan: `docs/benchmarks/moe-128k-suite-plan.md`).")
a("Brain (compose `engine`) stopped for the run, restored + verified after.")
a("")
a("## Sizing")
a("")
a("| r (memory-ratio) | free after load (MiB) | resolved (from the engine log) |")
a("|---|---|---|")
a(f"| {r} | {free} ({calib}) | {eng.splitlines()[-1] if eng else '?'} |")
a("")
a("Engine lines (verbatim):")
a("")
a("```")
a(eng)
a("```")
a("")
a("## Standard 3-run prompt sweep")
a("")
a("| run | prompt tok | prompt tok/s | gen tok | gen tok/s | total |")
a("|---|---|---|---|---|---|")
for run in std.get("runs", []):
    if "error" in run:
        a(f"| {run['run']} | ERROR {run['error']} | | | | |")
    else:
        a(f"| {run['run']} | {run['prompt_tokens']} | {run['prompt_tok_s']} | {run['gen_tokens']} | {run['gen_tok_s']} | {run['total_s']} s |")
if not std.get("runs"):
    a("| — | no runs completed (endpoint unreachable) | | | | |")
a("")
a("## Coding battery")
a("")
a(f"**{ex.get('pass_count', 0)}/{ex.get('total', 0)} passed**")
a("")
for e in ex.get("exercises", []):
    mark = "PASS" if e.get("passed") else f"FAIL ({e.get('error') or 'tests failed'})"
    a(f"- {e.get('name')}: {mark}")
if not ex.get("exercises"):
    a("- no exercises ran (endpoint unreachable)")
a("")
a("## 128K needle")
a("")
a(f"**{nd.get('pass_count', 0)}/{nd.get('total', 0)} positions correct**")
a("")
for p in nd.get("positions", []):
    a(f"- {p.get('position')}: {p.get('result', p)}")
if not nd.get("positions"):
    a("- no positions ran (endpoint unreachable)")
a("")
a("## Raw data")
a("")
a(f"JSONs + runner output + the ft serve log in")
a(f"`~/.local/share/jarvis/bench/moe-128k/{slug}-{target}/` (`standard.json`,")
a("`exercises.json`, `needle.json`, `*.out`, `ft-serve.log`, `point.log`).")
open(doc, "w").write("\n".join(lines) + "\n")
EOF
[[ -s "$DOC" ]] || { log "warning: results doc was not written"; }
cp "$DOC" "$OUT/point-doc.md" 2>/dev/null || true

# A machine-readable one-liner of the component results for the SUMMARY.
ST_RES=$(python3 -c "
import json
d = json.load(open('$OUT/standard.json'))
runs = [r for r in d.get('runs', []) if 'error' not in r]
print(f\"standard={len(runs)}/3\" + (f\" (gen {runs[-1]['gen_tok_s']} tok/s)\" if runs else \" (all errored)\"))" 2>/dev/null || echo "standard=?")
EX_RES=$(python3 -c "
import json
d = json.load(open('$OUT/exercises.json'))
print(f'exercises={d.get(\"pass_count\",0)}/{d.get(\"total\",0)} passed')" 2>/dev/null || echo "exercises=?")
ND_RES=$(python3 -c "
import json
d = json.load(open('$OUT/needle.json'))
print(f'needle={d.get(\"pass_count\",0)}/{d.get(\"total\",0)} positions correct')" 2>/dev/null || echo "needle=?")

log "point complete: $ST_RES, $EX_RES, $ND_RES — doc: $DOC"
echo "RESULTS $ST_RES | $EX_RES | $ND_RES | doc=$DOC" | tee -a "$OUT/point.log"
exit 0
