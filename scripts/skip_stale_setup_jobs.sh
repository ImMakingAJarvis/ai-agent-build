#!/usr/bin/env bash
# Skip leftover TASKS.md phase-* setup jobs in the agent store.
# Skips phase-1, 2 (not 2b), 3, 3b, 4, 5, 6.
# Keeps C/N/H/J0/D/S/Q/E/R and phase-2b.
# agent cannot read /home/nathan:
#   cp scripts/skip_stale_setup_jobs.sh /tmp/skip_stale_setup_jobs.sh
#   chmod 755 /tmp/skip_stale_setup_jobs.sh
#   sudo -u agent bash /tmp/skip_stale_setup_jobs.sh
set -euo pipefail
dir="${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}/jobs"
if [[ ! -d "$dir" ]]; then
  echo "skip_stale_setup_jobs: no store at $dir" >&2
  exit 1
fi
n=0
shopt -s nullglob
for d in "$dir"/*; do
  [[ -d "$d" ]] || continue
  id=$(basename "$d")
  case "$id" in
    phase-2b-*) continue ;;
    phase-1-*|phase-2-*|phase-3-*|phase-3b-*|phase-4-*|phase-5-*|phase-6-*) ;;
    *) continue ;;
  esac
  jarvis-jobs skip "$id"
  echo "skipped $id"
  n=$((n+1))
done
echo "skipped $n setup jobs"
if next=$(jarvis-jobs next 2>/dev/null); then
  echo "next: $next"
else
  echo "next: (none ready)"
fi
