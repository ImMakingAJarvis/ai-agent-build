#!/usr/bin/env bash
# Skip leftover Phase 1 / 2 / 3 setup jobs in the agent job store.
# Not 2b, not 3b, not C/N/H/J0/D/S/Q/E/R ids.
# agent cannot read /home/nathan. Copy first:
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
    phase-2b-*|phase-3b-*) continue ;;
    phase-1-*|phase-2-*|phase-3-*) ;;
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
