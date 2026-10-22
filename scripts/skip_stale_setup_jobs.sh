#!/usr/bin/env bash
# Skip leftover Phase 1 / Phase 2 jobs only (not 2b, not Phase 3).
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
for d in "$dir"/phase-1-* "$dir"/phase-2-*; do
  [[ -d "$d" ]] || continue
  id=$(basename "$d")
  case "$id" in
    phase-2b-*) continue ;;
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
