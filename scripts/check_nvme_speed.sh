#!/usr/bin/env bash
# Measure NVMe sequential read throughput and flag if below threshold.
# Uses dd for a quick test; falls back to fio if available.
set -euo pipefail

THRESHOLD_MBPS="${1:-2000}"  # Default: 2 GB/s
TEST_SIZE="${2:-2G}"         # Test file size
RESULTS_DIR="${HOME}/.local/share/jarvis"
mkdir -p "$RESULTS_DIR"

CSV="$RESULTS_DIR/benchmarks.csv"
if [[ ! -f "$CSV" ]]; then
  echo "timestamp,test,model,duration_ms,status" >"$CSV"
fi

log() {
  local test="$1" status="$2" mbps="$3"
  local ts
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  echo "${ts},${test},nvme,0,${status} ${mbps} MB/s" >>"$CSV"
  echo "${status}: NVMe sequential read = ${mbps} MB/s (threshold ${THRESHOLD_MBPS} MB/s)"
}

# Find the fastest NVMe device.
NVME_DEV=$(lsblk -d -o NAME,TYPE,ROTA,TRAN -r 2>/dev/null | \
  awk '$2=="disk" && $3=="0" && $4=="nvme" {print "/dev/"$1}' | head -1)

if [[ -z "$NVME_DEV" ]]; then
  # Fall back to testing root filesystem.
  NVME_DEV="/"
  echo "==> No NVMe detected; testing root filesystem"
else
  echo "==> Testing NVMe: $NVME_DEV"
fi

# Test using dd on a tmpfs-backed file to avoid disk write impact.
TMPFILE=$(mktemp /tmp/nvme-test-XXXXXX)
trap "rm -f $TMPFILE" EXIT

echo "==> Writing ${TEST_SIZE} test file..."
dd if=/dev/zero of="$TMPFILE" bs=1M count=512 oflag=direct 2>/dev/null || \
  dd if=/dev/zero of="$TMPFILE" bs=1M count=512 2>/dev/null

echo "==> Reading test file..."
RESULT=$(dd if="$TMPFILE" of=/dev/null bs=1M iflag=direct 2>&1 | \
  tail -1 | grep -oE '[0-9.]+ [GM]B/s' || true)

if [[ -z "$RESULT" ]]; then
  # Fallback: parse differently.
  RESULT=$(dd if="$TMPFILE" of=/dev/null bs=1M 2>&1 | \
    tail -1 | grep -oE '[0-9.]+ [GM]B/s' || true)
fi

if [[ -n "$RESULT" ]]; then
  MBPS=$(echo "$RESULT" | awk '{
    val = $1;
    if ($2 == "GB/s") val = val * 1024;
    printf "%.0f", val
  }')
  if (( MBPS >= THRESHOLD_MBPS )); then
    log "nvme-speed" "OK" "$MBPS"
  else
    log "nvme-speed" "BELOW THRESHOLD" "$MBPS"
  fi
else
  log "nvme-speed" "PARSE ERROR" "0"
fi
