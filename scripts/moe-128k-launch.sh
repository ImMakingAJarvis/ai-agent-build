#!/usr/bin/env bash
# moe-128k-launch.sh — start one MoE 128K ladder point DETACHED.
#
# The iGPU rounds are too small to babysit the 30-90 min point script
# (docs/benchmarks/moe-128k-suite-plan.md, "Decomposition"). This script is
# the whole job of the launcher round: it preflights, starts
# moe-128k-point.sh under setsid+nohup (it survives the round ending),
# waits until the brain is actually stopped (so the job loop cannot start a
# brain round onto a mid-stop engine), and prints one LAUNCH* line.
#
# The point script's EXIT trap calls moe-128k-done.sh (the done hook), which
# fires the checker job on OK and the sonnet fallback job on failure — a dead
# launcher round cannot therefore strand the point.
#
# Usage:  scripts/moe-128k-launch.sh <model-slug> <target-free-MB> [short]
#   short  qwen | glm   (job-id prefix; default derived from the slug)
#
# Job ids (env overrides; empty = that firing is disabled):
#   MOE_RUN_JOB     default moe128k-<short>-<mb>-run     (this round's job)
#   MOE_CHECKER_JOB default moe128k-<short>-<mb>-check
#   MOE_SONNET_JOB  default moe128k-<short>-<mb>-sonnet
#
# Exit 0 = the point is in flight (LAUNCHED / ALREADY_RUNNING / LAUNCH_PENDING)
# Exit 1 = the point never started (LAUNCH_FAILED); the sonnet fallback job
#          is requeued by this script if it is parked.
set -u

SLUG="${1:?usage: scripts/moe-128k-launch.sh <model-slug> <target-free-MB> [short]}"
TARGET_MB="${2:?usage: scripts/moe-128k-launch.sh <model-slug> <target-free-MB> [short]}"
SHORT="${3:-}"
case "$SLUG" in
  qwen3.8-flash-next) SHORT="${SHORT:-qwen}" ;;
  glm5.3-flash)       SHORT="${SHORT:-glm}" ;;
  *) echo "LAUNCH_FAILED reason=unknown model slug: $SLUG" >&2; exit 1 ;;
esac
case "$TARGET_MB" in 400|300|250|200|150|100) ;;
  *) echo "LAUNCH_FAILED reason=unknown ladder target: $TARGET_MB" >&2; exit 1 ;;
esac

REPO=/home/agent/ai-agent-build
POINT=$REPO/scripts/moe-128k-point.sh
OUT="$HOME/.local/share/jarvis/bench/moe-128k/${SLUG}-${TARGET_MB}"
PIDF=$OUT/point.pid
LAUNCH_PIDF=$OUT/launch.pid
mkdir -p "$OUT"

MOE_RUN_JOB="${MOE_RUN_JOB:-moe128k-${SHORT}-${TARGET_MB}-run}"
MOE_CHECKER_JOB="${MOE_CHECKER_JOB:-moe128k-${SHORT}-${TARGET_MB}-check}"
MOE_SONNET_JOB="${MOE_SONNET_JOB:-moe128k-${SHORT}-${TARGET_MB}-sonnet}"
export MOE_RUN_JOB MOE_CHECKER_JOB MOE_SONNET_JOB
export MOE_DONE_HOOK="$REPO/scripts/moe-128k-done.sh"

job_status() { jarvis-jobs show "$1" 2>/dev/null | sed -n 's/^status: //p'; }

fire_if_parked() {  # $1 = job id; requeue when parked or failed, never otherwise
  [[ -n "$1" ]] || return 0
  case "$(job_status "$1")" in
    parked|failed) jarvis-jobs requeue "$1" 2>/dev/null && echo "requeued $1" >&2 ;;
  esac
}

# ------------------------------------------------------------- idempotent --
if [[ -f "$LAUNCH_PIDF" ]] && kill -0 "$(cat "$LAUNCH_PIDF" 2>/dev/null)" 2>/dev/null; then
  echo "ALREADY_RUNNING pid=$(cat "$LAUNCH_PIDF") slug=$SLUG target=${TARGET_MB}mb"
  exit 0
fi

# ---------------------------------------------------------------- preflight --
# :11436 busy means another ft serve (another point) is live; never fight it.
if curl -sf http://127.0.0.1:11436/v1/models >/dev/null 2>&1; then
  echo "LAUNCH_FAILED reason=:11436 already answers (another ft serve is running)"
  touch "$OUT/launch-failed"
  fire_if_parked "$MOE_SONNET_JOB"
  exit 1
fi
# The brain must be up (the point stops it). Retry: a photo swap window or a
# transient engine blip should ride out, not kill the launch. 10 x 60 s.
i=0
until curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1; do
  i=$((i + 1))
  if (( i >= 10 )); then
    echo "LAUNCH_FAILED reason=brain (:11434) not up after 10 min"
    touch "$OUT/launch-failed"
    fire_if_parked "$MOE_SONNET_JOB"
    exit 1
  fi
  sleep 60
done

# A new attempt gets a fresh point.log; the previous attempt's log is kept.
if [[ -f "$OUT/point.log" ]]; then
  mv "$OUT/point.log" "$OUT/point.log.prev-$(date +%s)"
fi
: > "$OUT/point.log"
rm -f "$OUT/launch-failed"

# --------------------------------------------------------------- the launch --
# 3 h hard cap: the suite runners are otherwise unbounded, and a hung point
# would hold the brain stopped past the watchdog window. TERM (not KILL) so
# the point's EXIT trap can kill ft and restore the brain; -k 900 gives that
# cleanup 15 min before the kill lands.
PATH=/home/agent/ftvenv/bin:$PATH setsid nohup \
  timeout -k 900 3h bash "$POINT" "$SLUG" "$TARGET_MB" >>"$OUT/point.log" 2>&1 &
LPID=$!
echo "$LPID" > "$LAUNCH_PIDF"

# Wait until the point has stopped the brain ("VRAM freed" in its log) or
# died. By the time the launcher round ends, no local (brain) round can
# start, because the loop skips jobs whose engine is down.
state=""
for i in $(seq 1 15); do
  if grep -q "VRAM freed" "$OUT/point.log" 2>/dev/null; then state=launched; break; fi
  if grep -q "^SUMMARY\|POINT FAILED" "$OUT/point.log" 2>/dev/null; then state=pointlog; break; fi
  kill -0 "$LPID" 2>/dev/null || { state=dead; break; }
  sleep 60
done
[[ -z "$state" ]] && state=pending

case "$state" in
  launched)
    echo "LAUNCHED pid=$LPID slug=$SLUG target=${TARGET_MB}mb log=$OUT/point.log"
    exit 0 ;;
  pointlog)
    # The point already wrote its verdict before we saw it (very fast fail).
    tail -n 5 "$OUT/point.log"
    if grep -q "SUMMARY moe-128k.*: OK" "$OUT/point.log"; then
      echo "LAUNCHED pid=$LPID slug=$SLUG target=${TARGET_MB}mb log=$OUT/point.log (already finished fast)"
      exit 0
    fi
    echo "LAUNCH_FAILED reason=point exited before VRAM was freed (see tail above)"
    touch "$OUT/launch-failed"
    fire_if_parked "$MOE_SONNET_JOB"
    exit 1 ;;
  dead)
    tail -n 10 "$OUT/point.log"
    echo "LAUNCH_FAILED reason=point script died during preflight (see tail above)"
    touch "$OUT/launch-failed"
    fire_if_parked "$MOE_SONNET_JOB"
    exit 1 ;;
  pending)
    # Still in preflight (brain not up, e.g. a long photo window). The point
    # decides for itself; the checker round will wait on its SUMMARY.
    echo "LAUNCH_PENDING pid=$LPID slug=$SLUG target=${TARGET_MB}mb log=$OUT/point.log (point still in preflight)"
    exit 0 ;;
esac
