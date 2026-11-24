#!/usr/bin/env bash
# moe-128k-check.sh — verify one MoE 128K ladder point landed.
#
# The checker round runs this ONE command (it may block for up to 4 h while
# the detached point finishes — that is the design; there is nothing else to
# do in the round). It prints one VERDICT= line plus the evidence the round
# needs for the board / Discord / job finish:
#
#   VERDICT=OK            the point completed; RESULTS + artifact list follow
#   VERDICT=FAILED        the point ran and its SUMMARY says FAILED (reason + tail)
#   VERDICT=LAUNCH_FAILED the launcher reported the point never started
#   VERDICT=NEVER_LAUNCHED no launch within 30 min (dead or narrated launcher)
#   VERDICT=NO_SUMMARY    the point process died without a SUMMARY (hard kill)
#   VERDICT=STILL_RUNNING the 4 h wait expired while the point is alive
#
# It also prints SONNET=<status> for the fallback job so the round requeues
# it only when it is actually parked/failed (the point's done-hook may have
# already fired it).
#
# Usage:  scripts/moe-128k-check.sh <model-slug> <target-free-MB> [short]
# Exit 0 = OK, 1 = anything else.
set -u

SLUG="${1:?usage: scripts/moe-128k-check.sh <model-slug> <target-free-MB> [short]}"
TARGET_MB="${2:?usage: scripts/moe-128k-check.sh <model-slug> <target-free-MB> [short]}"
SHORT="${3:-}"
case "$SLUG" in
  qwen3.8-flash-next) SHORT="${SHORT:-qwen}" ;;
  glm5.3-flash)       SHORT="${SHORT:-glm}" ;;
  *) echo "unknown model slug: $SLUG" >&2; exit 2 ;;
esac

OUT="$HOME/.local/share/jarvis/bench/moe-128k/${SLUG}-${TARGET_MB}"
LOGF=$OUT/point.log
PIDF=$OUT/point.pid
MOE_SONNET_JOB="${MOE_SONNET_JOB:-moe128k-${SHORT}-${TARGET_MB}-sonnet}"

sonnet_status() { jarvis-jobs show "$MOE_SONNET_JOB" 2>/dev/null | sed -n 's/^status: //p'; }
verdict() {  # $1 = verdict, $2 = exit code
  echo "SONNET=$(sonnet_status)"
  echo "VERDICT=$1"
  exit "$2"
}

pid_alive() { [[ -f "$PIDF" ]] && kill -0 "$(cat "$PIDF" 2>/dev/null)" 2>/dev/null; }
has_summary() { grep -q "^SUMMARY moe-128k" "$LOGF" 2>/dev/null; }

# No log yet: the launcher may simply not have run (a dead or narrating
# launcher round). Give it 30 min, then call it never-launched.
if [[ ! -f "$LOGF" ]]; then
  if [[ -f "$OUT/launch-failed" ]]; then
    verdict LAUNCH_FAILED 1
  fi
  echo "no point.log yet — waiting up to 30 min for the launcher"
  for i in $(seq 1 30); do
    [[ -f "$LOGF" ]] && break
    sleep 60
  done
  if [[ ! -f "$LOGF" ]]; then
    # The launcher may have failed its preflight while we waited: it
    # touches launch-failed before exiting. Report the real reason.
    [[ -f "$OUT/launch-failed" ]] && verdict LAUNCH_FAILED 1
    verdict NEVER_LAUNCHED 1
  fi
fi

# Keep this attempt's log under a stable name so a fallback re-run cannot
# overwrite the evidence of this attempt.
cp -a "$LOGF" "$OUT/point.log.igpu-attempt" 2>/dev/null || true

# Block until the SUMMARY lands (or the 4 h backstop: the point itself is
# capped at 3 h + 15 min of cleanup, so a live pid past this is anomalous).
if ! has_summary; then
  echo "point in flight — waiting for its SUMMARY (up to 4 h)"
  for i in $(seq 1 240); do
    has_summary && break
    sleep 60
  done
fi

if ! has_summary; then
  if pid_alive; then
    tail -n 10 "$LOGF"
    verdict STILL_RUNNING 1
  fi
  echo "point process is gone and point.log has no SUMMARY line"
  tail -n 15 "$LOGF"
  verdict NO_SUMMARY 1
fi

SUMMARY_LINE="$(grep "^SUMMARY moe-128k" "$LOGF" | tail -1)"
echo "$SUMMARY_LINE"
RESULTS_LINE="$(grep "^RESULTS" "$LOGF" | tail -1)"
[[ -n "$RESULTS_LINE" ]] && echo "$RESULTS_LINE"

if [[ "$SUMMARY_LINE" == *:OK* ]]; then
  echo "artifacts in $OUT:"
  ls -la "$OUT" | grep -E "standard.json|exercises.json|needle.json|point-doc.md|point.log$" || true
  DOC="/home/agent/ai-agent-build/docs/benchmarks/moe-128k-${SLUG}-${TARGET_MB}mb.md"
  [[ -s "$DOC" ]] && echo "doc=$DOC" || echo "doc=MISSING ($DOC)"
  verdict OK 0
fi

echo "point log tail:"
tail -n 15 "$LOGF"
verdict FAILED 1
