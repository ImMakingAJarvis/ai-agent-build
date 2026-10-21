#!/usr/bin/env bash
# Health-check: confirm Ollama is running and the configured model responds
# to a trivial prompt. Exits 0 on success, 1 on failure.
set -euo pipefail

source "${HOME}/.config/jarvis/local.env" 2>/dev/null || true

LIGHT_MODEL="${LOCAL_MODEL:-jarvis-brain}"
HEAVY_MODEL="${LOCAL_CODER_MODEL:-}"
OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"

fail() { echo "FAIL: $1" >&2; exit 1; }

# Check Ollama daemon is responding.
curl -fsS "${OLLAMA_URL}/" >/dev/null 2>&1 || fail "Ollama daemon not responding at ${OLLAMA_URL}"
echo "OK: Ollama daemon is running"

# Check light model responds.
RESPONSE=$(ollama run "$LIGHT_MODEL" "Reply with exactly: OK" --quiet 2>/dev/null) || \
  fail "Light model ${LIGHT_MODEL} did not respond"
echo "OK: Light model ${LIGHT_MODEL} responded: ${RESPONSE:0:50}"

# Check heavy model responds (if configured).
if [[ -n "$HEAVY_MODEL" ]]; then
  RESPONSE=$(ollama run "$HEAVY_MODEL" "Reply with exactly: OK" --quiet 2>/dev/null) || \
    fail "Heavy model ${HEAVY_MODEL} did not respond"
  echo "OK: Heavy model ${HEAVY_MODEL} responded: ${RESPONSE:0:50}"
else
  echo "SKIP: No heavy model configured"
fi

# Check RabbitMQ is reachable.
if command -v docker >/dev/null 2>&1; then
  if sudo docker ps --filter 'name=jarvis-rabbitmq' --filter 'status=running' -q | grep -q .; then
    echo "OK: RabbitMQ container is running"
  else
    echo "WARN: RabbitMQ container not running"
  fi
fi

echo "==> All health checks passed"
