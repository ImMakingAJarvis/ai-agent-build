#!/usr/bin/env bash
# moe-128k-done.sh — the done hook for one MoE 128K ladder point.
#
# Called by moe-128k-point.sh's EXIT trap after the SUMMARY line is written,
# from the detached point process — NOT from any model round. This is the
# error handler that keeps the chain moving when a round is dead: a launcher
# or checker round can OOM or thrash its context and never finish, but the
# point itself is code and its trap always runs.
#
#   rc = 0 (point OK)      -> requeue the CHECKER job (it verifies + records)
#   rc != 0 (point failed) -> requeue the SONNET fallback job (it records the
#                             iGPU failure, then re-runs the point on Sonnet)
#
# A job is only requeued when it is parked or failed — never when it is
# ready, claimed or running (it will see the SUMMARY itself), so the hook can
# fire at most once per job per attempt.
#
# Usage:  scripts/moe-128k-done.sh <model-slug> <target-free-MB> <rc>
# Env:    MOE_CHECKER_JOB / MOE_SONNET_JOB (set by the launcher; empty = off)
# Always exits 0: the hook must never change the point's own result.
set -u

SLUG="${1:?usage: scripts/moe-128k-done.sh <model-slug> <target-free-MB> <rc>}"
TARGET_MB="${2:?usage: scripts/moe-128k-done.sh <model-slug> <target-free-MB> <rc>}"
RC="${3:?usage: scripts/moe-128k-done.sh <model-slug> <target-free-MB> <rc>}"

CHECKER="${MOE_CHECKER_JOB:-}"
SONNET="${MOE_SONNET_JOB:-}"

job_status() { jarvis-jobs show "$1" 2>/dev/null | sed -n 's/^status: //p'; }

fire_if_parked() {  # $1 = job id
  [[ -n "$1" ]] || return 0
  local st
  st="$(job_status "$1")"
  case "$st" in
    parked|failed)
      echo "[hook] requeueing $1 (was $st)"
      jarvis-jobs requeue "$1" || echo "[hook] WARNING: requeue $1 failed" ;;
    *)
      echo "[hook] $1 is $st — not touching it" ;;
  esac
}

echo "[hook] moe-128k done: $SLUG ${TARGET_MB}mb rc=$RC"
if [[ "$RC" == "0" ]]; then
  fire_if_parked "$CHECKER"
else
  fire_if_parked "$SONNET"
fi
exit 0
