#!/usr/bin/env bash
# Benchmark model swap latencies: cold load, swap, and warm generation.
# Logs results to CSV at ~/.local/share/jarvis/benchmarks.csv.
set -euo pipefail

source "${HOME}/.config/jarvis/local.env" 2>/dev/null || true

LIGHT_MODEL="${LOCAL_MODEL:-jarvis-brain}"
HEAVY_MODEL="${LOCAL_CODER_MODEL:-}"
CSV="${HOME}/.local/share/jarvis/benchmarks.csv"

mkdir -p "$(dirname "$CSV")"

# Write header if file doesn't exist.
if [[ ! -f "$CSV" ]]; then
  echo "timestamp,test,model,duration_ms,status" >"$CSV"
fi

log() {
  local test="$1" model="$2" duration_ms="$3" status="$4"
  local ts
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  echo "${ts},${test},${model},${duration_ms},${status}" >>"$CSV"
  echo "${status}: ${test} ${model} = ${duration_ms}ms"
}

# --- Test 1: Light model cold load time ---
echo "==> Unloading light model for cold load test"
curl -s -X POST http://localhost:11434/api/pull -d "{\"name\":\"${LIGHT_MODEL}\",\"stream\":false}" >/dev/null
# Force unload by setting keep_alive to 0
curl -s -X POST http://localhost:11434/api/generate -d \
  "{\"model\":\"${LIGHT_MODEL}\",\"prompt\":\".\",\"stream\":false,\"options\":{\"keep_alive\":0}}" >/dev/null 2>&1
sleep 2

echo "==> Timing light model cold load"
START=$(date +%s%N)
curl -s -X POST http://localhost:11434/api/generate -d \
  "{\"model\":\"${LIGHT_MODEL}\",\"prompt\":\"Say OK\",\"stream\":false}" >/dev/null
END=$(date +%s%N)
DUR=$(( (END - START) / 1000000 ))
log "cold-load-light" "$LIGHT_MODEL" "$DUR" "OK"

# --- Test 2: Light model warm generation (no load penalty) ---
echo "==> Timing light model warm generation"
START=$(date +%s%N)
curl -s -X POST http://localhost:11434/api/generate -d \
  "{\"model\":\"${LIGHT_MODEL}\",\"prompt\":\"Say OK\",\"stream\":false}" >/dev/null
END=$(date +%s%N)
DUR=$(( (END - START) / 1000000 ))
log "warm-gen-light" "$LIGHT_MODEL" "$DUR" "OK"

# --- Test 3: Heavy model cold load (if configured) ---
if [[ -n "$HEAVY_MODEL" ]]; then
  echo "==> Timing heavy model cold load"
  START=$(date +%s%N)
  curl -s -X POST http://localhost:11434/api/generate -d \
    "{\"model\":\"${HEAVY_MODEL}\",\"prompt\":\"Say OK\",\"stream\":false}" >/dev/null
  END=$(date +%s%N)
  DUR=$(( (END - START) / 1000000 ))
  log "cold-load-heavy" "$HEAVY_MODEL" "$DUR" "OK"

  # --- Test 4: Swap back to light model ---
  echo "==> Timing swap back to light model"
  START=$(date +%s%N)
  curl -s -X POST http://localhost:11434/api/generate -d \
    "{\"model\":\"${LIGHT_MODEL}\",\"prompt\":\"Say OK\",\"stream\":false}" >/dev/null
  END=$(date +%s%N)
  DUR=$(( (END - START) / 1000000 ))
  log "swap-to-light" "$LIGHT_MODEL" "$DUR" "OK"
else
  echo "==> No heavy model configured; skipping swap tests"
  log "cold-load-heavy" "-" 0 "SKIPPED"
  log "swap-to-light" "-" 0 "SKIPPED"
fi

echo "==> Results logged to $CSV"
