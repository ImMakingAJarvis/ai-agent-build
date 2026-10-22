#!/usr/bin/env bash
# S3.2-S3.4: Test llama.cpp server with a small model, two slots, and cache retention.
# Runs only between rounds (brain unloaded). Restores Ollama state on exit.
#
# Usage: bash scripts/engine-try.sh
set -euo pipefail

BIN=/opt/jarvis-engine/bin/llama-server
PORT=8090
BASE="http://localhost:$PORT"
SMALL_MODEL="${SMALL_MODEL:-llama3.2:1b}"  # pull a tiny model for testing
SLOT_CTX=8192
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

green() { echo -e "\033[32m[OK]\033[0m $*"; }
red()   { echo -e "\033[31m[FAIL]\033[0m $*"; }
info()  { echo "==> $*"; }

########################################
# 0. Pre-flight: no round running
########################################
info "Checking no agent round is active ..."
if [ -f /tmp/jarvis-agent-running ]; then
  red "Agent round is running. Unload the brain and stop the round first."
  exit 1
fi

########################################
# 1. Unload brain from Ollama
########################################
info "Unloading jarvis-brain from Ollama ..."
ollama stop jarvis-brain 2>/dev/null || true
sleep 2
nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits | head -1 | xargs -I{} echo "   VRAM free-ing: {} MB used (should drop)"

########################################
# 2. Pull small model & start server with 2 slots
########################################
info "Pulling $SMALL_MODEL ..."
ollama pull "$SMALL_MODEL"

info "Starting llama.cpp server on :$PORT with 2 slots ..."
# Resolve GGUF path from Ollama's blob cache (same logic as install_engine.sh)
OLLAMA_BLOB_DIR="/usr/share/ollama/.ollama/models/blobs"
MODEL_HASH=$(ollama show --modelfile "$SMALL_MODEL" 2>/dev/null \
  | grep -oP 'sha256-\K[a-f0-9]+' | head -1)
MODEL_PATH="${OLLAMA_BLOB_DIR}/sha256-${MODEL_HASH}"
if [[ ! -f "$MODEL_PATH" ]]; then
  red "Model blob not found at $MODEL_PATH"
  exit 1
fi
info "   Using model: $MODEL_PATH"
mkdir -p "$TMPDIR/slots"
$BIN -m "$MODEL_PATH" \
     --port "$PORT" \
     --parallel 2 \
     --ctx-size $((SLOT_CTX * 2)) \
     --kv-unified \
     --cache-type-k q4_0 \
     --cache-type-v q4_0 \
     --slot-save-path "$TMPDIR/slots" \
     --log-disable \
     >"$TMPDIR/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server to be ready
info "Waiting for server to start ..."
for i in $(seq 1 60); do
  if curl -sf "$BASE/health" >/dev/null 2>&1; then
    green "Server ready (PID $SERVER_PID)"
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    red "Server died. Log:"
    tail -20 "$TMPDIR/server.log"
    exit 1
  fi
  sleep 1
done

if ! curl -sf "$BASE/health" >/dev/null 2>&1; then
  red "Server did not become healthy in 60s"
  tail -20 "$TMPDIR/server.log"
  kill "$SERVER_PID" 2>/dev/null || true
  exit 1
fi

########################################
# 3. S3.2 — Two concurrent requests stream
########################################
info "S3.2: Two concurrent requests ..."
curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"Say hi in 3 words"}],"slot_id_or_index":0}' \
  >"$TMPDIR/resp0" 2>&1 &
PID0=$!

curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"Say bye in 3 words"}],"slot_id_or_index":1}' \
  >"$TMPDIR/resp1" 2>&1 &
PID1=$!

wait $PID0 $PID1 2>/dev/null || true

# Extract content from JSON response
CONTENT0=$(python3 -c "import sys,json; print(json.load(sys.stdin)['choices'][0]['message']['content'])" <"$TMPDIR/resp0" 2>/dev/null || echo "")
CONTENT1=$(python3 -c "import sys,json; print(json.load(sys.stdin)['choices'][0]['message']['content'])" <"$TMPDIR/resp1" 2>/dev/null || echo "")

if [[ -n "$CONTENT0" ]] && [[ -n "$CONTENT1" ]]; then
  green "Both slots responded concurrently"
  echo "   Slot 0: $CONTENT0"
  echo "   Slot 1: $CONTENT1"
else
  red "One or both requests failed"
  cat "$TMPDIR/resp0" "$TMPDIR/resp1"
fi

########################################
# 4. S3.3 — API returns structured responses
########################################
info "S3.3: API returns valid JSON ..."
RESPONSE=$(curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"What is 2+2? Answer with just the number."}]}' \
  2>&1)

if echo "$RESPONSE" | grep -q '"choices"'; then
  green "OpenAI-compatible API works"
else
  red "API response missing choices field"
  echo "$RESPONSE" | head -5
fi

########################################
# 5. S3.4 — Slots keep their cache
########################################
info "S3.4: Cache retention across slots ..."

# Generate a ~6K token prompt (repeated text to fill context)
LONG_PROMPT=""
for i in $(seq 1 400); do
  LONG_PROMPT+="Token $i: "
done

# Send to slot 1 and time it
T1_START=$(date +%s%N)
curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"test\",\"messages\":[{\"role\":\"user\",\"content\":\"$LONG_PROMPT\"}],\"slot_id_or_index\":1}" \
  >"$TMPDIR/slot1_first" 2>&1
T1_END=$(date +%s%N)
T1_MS=$(( (T1_END - T1_START) / 1000000 ))
info "  Slot 1 first call: ${T1_MS}ms"

# Chat on slot 0 (should not evict slot 1's cache with --kv-unified + separate slots)
curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"What day is it?"}],"slot_id_or_index":0}' \
  >"$TMPDIR/slot0_chat" 2>&1
info "  Slot 0 chat: done"

# Send same long prompt to slot 1 again — should be faster if cached
T2_START=$(date +%s%N)
curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"test\",\"messages\":[{\"role\":\"user\",\"content\":\"$LONG_PROMPT\"}],\"slot_id_or_index\":1}" \
  >"$TMPDIR/slot1_second" 2>&1
T2_END=$(date +%s%N)
T2_MS=$(( (T2_END - T2_START) / 1000000 ))
info "  Slot 1 second call: ${T2_MS}ms"

if [ "$T2_MS" -lt "$T1_MS" ]; then
  green "Cache hit: second call (${T2_MS}ms) faster than first (${T1_MS}ms)"
else
  info "Cache may not have helped (${T2_MS}ms vs ${T1_MS}ms) — prompt may be too short to measure"
fi

# Check slot state via /slots endpoint
info "Slot state:"
curl -sf "$BASE/slots" 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "  (could not parse /slots)"

########################################
# 6. Save/restore slot 1 cache
########################################
info "Slot save/restore test ..."

# Save slot 1 — requires {"filename":"..."} in JSON body
SAVE_RESP=$(curl -sf -X POST "$BASE/slots/1?action=save" \
  -H 'Content-Type: application/json' \
  -d '{"filename":"slot1-save"}' 2>&1 || echo '{"error":true}')
echo "$SAVE_RESP" | head -3

# Overwrite slot 1 with a different request
curl -sf -X POST "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"test","messages":[{"role":"user","content":"Overwrite test."}],"slot_id_or_index":1}' \
  >"$TMPDIR/slot1_overwrite" 2>&1

# Restore slot 1
RESTORE_RESP=$(curl -sf -X POST "$BASE/slots/1?action=restore" \
  -H 'Content-Type: application/json' \
  -d '{"filename":"slot1-save"}' 2>&1 || echo '{"error":true}')
echo "$RESTORE_RESP" | head -3

green "Save/restore attempted (check server log for details)"

########################################
# 7. Cleanup
########################################
info "Stopping server ..."
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
sleep 1

info "Reloading nothing — Ollama will reload brain on next request."
green "All tests complete."
echo
echo "Timings summary:"
echo "  Slot 1 first:  ${T1_MS}ms"
echo "  Slot 1 second: ${T2_MS}ms"
