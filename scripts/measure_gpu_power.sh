#!/bin/bash
# Measure GPU power: idle with model loaded in VRAM vs generating
set -e

OUTPUT_DIR="/tmp/gpu_power_measurements"
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

SAMPLE_INTERVAL=1  # seconds between samples

echo "=== GPU Power Measurement Script ==="

# Sample GPU stats continuously, writing to file
SAMPLE_FILE="$OUTPUT_DIR/all_samples.csv"
echo "timestamp,power_w,temperature_c,utilization_pct,memory_miB" > "$SAMPLE_FILE"

sample_gpu() {
    while true; do
        line=$(nvidia-smi --query-gpu=power.draw,temperature.gpu,utilization.gpu,memory.used --format=csv,noheader | tr -d ' ')
        ts=$(date +%s)
        echo "${ts},${line}" >> "$SAMPLE_FILE"
        sleep "$SAMPLE_INTERVAL"
    done
}

# Start sampler
sample_gpu &
SAMPLER_PID=$!
sleep 1

###############################################################################
# STEP 1: Load the model and wait until it's fully in VRAM
###############################################################################
echo "[1/4] Loading model into VRAM..."

# Fire off the API call in background so we can watch VRAM
curl -s -X POST http://localhost:11434/api/chat -d '{
  "model": "jarvis-brain",
  "keep_alive": 1800,
  "messages": [{"role": "user", "content": "OK"}],
  "stream": false
}' > /dev/null &
API_PID=$!

# Wait until VRAM shows the model is loaded (>15000 MiB = ~15 GB)
echo "  Waiting for model to load into VRAM..."
LOADED=0
for i in $(seq 1 120); do
    vram=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader | tr -d ' ' | sed 's/MiB//')
    if [ "$vram" -gt 15000 ] 2>/dev/null; then
        LOADED=1
        echo "  Model loaded! VRAM: ${vram} MiB"
        break
    fi
    sleep 2
done
wait $API_PID 2>/dev/null

if [ "$LOADED" -eq 0 ]; then
    echo "ERROR: Model did not load into VRAM"
    kill $SAMPLER_PID 2>/dev/null
    exit 1
fi

###############################################################################
# STEP 2: Idle measurement (model loaded, not generating)
###############################################################################
echo ""
echo "[2/4] Measuring IDLE power (model in VRAM, not generating)..."
echo "  Duration: 90 seconds"
IDLE_START=$(date +%s)

while true; do
    now=$(date +%s)
    elapsed=$((now - IDLE_START))
    if [ "$elapsed" -ge 90 ]; then
        break
    fi
    # Verify model is still loaded
    vram=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader | tr -d ' ' | sed 's/MiB//')
    if [ "$vram" -lt 10000 ] 2>/dev/null; then
        echo "  WARNING: Model appears to have unloaded at t=${elapsed}s (VRAM: ${vram} MiB)"
        break
    fi
    sleep 5
done

###############################################################################
# STEP 3: Generation measurement
###############################################################################
echo ""
echo "[3/4] Measuring GENERATION power..."
echo "  Sending long prompt..."

GEN_START=$(date +%s)

# Send a prompt that generates a long response
curl -s -X POST http://localhost:11434/api/chat -d '{
  "model": "jarvis-brain",
  "keep_alive": 1800,
  "stream": false,
  "messages": [
    {"role": "user", "content": "Write a very detailed technical explanation of how transformer neural networks work. Cover in depth: self-attention mechanism with the Q/K/V matrices, scaled dot-product attention, positional encodings, multi-head attention, the feed-forward network in each layer, residual connections, layer normalization, encoder-decoder architecture, and how training works with masked attention. Include mathematical formulas where relevant. Make this at least 3000 words."}
  ]
}' > /dev/null

GEN_END=$(date +%s)
GEN_TIME=$((GEN_END - GEN_START))
echo "  Generation completed in ${GEN_TIME}s"

# Wait a bit more to capture post-generation cooldown
sleep 15

###############################################################################
# STEP 4: Analyze results
###############################################################################
kill $SAMPLER_PID 2>/dev/null
wait $SAMPLER_PID 2>/dev/null

echo ""
echo "============================================"
echo "           RESULTS SUMMARY"
echo "============================================"
echo ""

# Extract idle period: samples where memory > 15000 and utilization < 10%
echo "--- IDLE (model loaded in VRAM, not generating) ---"
awk -F',' '
{
    mem = $5 + 0;
    util = $4 + 0;
    power = $2 + 0;
    # Model loaded (high VRAM) and idle (low utilization)
    if (mem > 15000 && util < 10) {
        sum += power; count++;
        if (count == 1 || power > max) max = power;
        if (count == 1 || power < min) min = power;
    }
}
END {
    if (count > 0) {
        printf "  Samples collected: %d (%.0fs)\n", count, count * 1;
        printf "  Average power: %.1f W\n", sum/count;
        printf "  Min power: %.1f W\n", min;
        printf "  Max power: %.1f W\n", max;
    } else {
        print "  No idle samples with model loaded found";
    }
}
' "$SAMPLE_FILE"

echo ""
echo "--- GENERATION (model actively producing tokens) ---"
awk -F',' '
{
    mem = $5 + 0;
    util = $4 + 0;
    power = $2 + 0;
    # Model loaded and actively generating (high utilization)
    if (mem > 15000 && util >= 50) {
        sum += power; count++;
        if (count == 1 || power > max) max = power;
        if (count == 1 || power < min) min = power;
    }
}
END {
    if (count > 0) {
        printf "  Samples collected: %d (%.0fs)\n", count, count * 1;
        printf "  Average power: %.1f W\n", sum/count;
        printf "  Min power: %.1f W\n", min;
        printf "  Max power: %.1f W\n", max;
    } else {
        print "  No generation samples found";
    }
}
' "$SAMPLE_FILE"

echo ""
echo "--- GPU BASELINE (no model loaded) ---"
awk -F',' '
{
    mem = $5 + 0;
    power = $2 + 0;
    if (mem < 5000) {
        sum += power; count++;
        if (count == 1 || power > max) max = power;
        if (count == 1 || power < min) min = power;
    }
}
END {
    if (count > 0) {
        printf "  Samples collected: %d\n", count;
        printf "  Average power: %.1f W\n", sum/count;
        printf "  Min power: %.1f W\n", min;
        printf "  Max power: %.1f W\n", max;
    }
}
' "$SAMPLE_FILE"

echo ""
echo "=== Full data: $SAMPLE_FILE ==="
