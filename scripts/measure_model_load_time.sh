#!/bin/bash
# Measure model load time from NVMe (cold start) for jarvis-brain
# Goal: confirm whether load time is too slow for post-wake-word use
set -euo pipefail

OUTPUT_DIR="/tmp/model_load_time"
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

MODEL="jarvis-brain"
RUNS=${1:-5}

RESULTS_FILE="$OUTPUT_DIR/load_times.csv"

echo "============================================"
echo "  Model Load Time Measurement (Cold Start)"
echo "  Model: $MODEL"
echo "  Runs: $RUNS"
echo "============================================"
echo ""

# Helper: high-res timestamp in seconds (float)
now_ts() {
    python3 -c "import time; print(f'{time.time():.3f}')"
}

# Helper: ensure model is unloaded
ensure_unloaded() {
    ollama stop "$MODEL" 2>/dev/null || true
    sleep 3
    local vram
    vram=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader | tr -d ' ' | sed 's/MiB//')
    echo "  VRAM: ${vram} MiB"
    if [ "$vram" -gt 5000 ]; then
        echo "  WARNING: Model still in VRAM (${vram} MiB), trying again..."
        ollama stop "$MODEL" 2>/dev/null || true
        sleep 5
        vram=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader | tr -d ' ' | sed 's/MiB//')
        echo "  VRAM after retry: ${vram} MiB"
    fi
}

# Ensure unloaded before we start
echo "Ensuring model is unloaded..."
ensure_unloaded

###############################################################################
# GPU sampler: background process sampling every 200ms
############################################################################>
run_gpu_sampler() {
    local outfile="$1"
    echo "timestamp_s,power_w,temperature_c,utilization_pct,memory_miB" > "$outfile"
    local start
    start=$(date +%s)
    while true; do
        local ts line
        ts=$(date +%s)
        local elapsed=$((ts - start))
        line=$(nvidia-smi --query-gpu=power.draw,temperature.gpu,utilization.gpu,memory.used --format=csv,noheader 2>/dev/null | tr -d ' ')
        echo "${elapsed},${line}" >> "$outfile"
        sleep 0.2
    done
}

###############################################################################
# Run each cold-start test
###############################################################################
echo ""
echo "run,load_time_s,vram_end_miB" > "$RESULTS_FILE"

for run in $(seq 1 $RUNS); do
    echo ""
    echo "--- Run $run/$RUNS ---"
    ensure_unloaded

    # Start GPU sampler
    TIMELINE_FILE="$OUTPUT_DIR/timeline_run_${run}.csv"
    run_gpu_sampler "$TIMELINE_FILE" &
    SAMPLER_PID=$!
    sleep 0.3

    # Record start time
    START=$(now_ts)

    # Fire the request - triggers the cold load
    echo "  Request sent at $(date +%T)"
    curl -s -X POST http://localhost:11434/api/chat -d '{
      "model": "'"$MODEL"'",
      "keep_alive": 3600,
      "messages": [{"role": "user", "content": "OK"}],
      "stream": false
    }' > "$OUTPUT_DIR/response_run_${run}.json" 2>&1

    END=$(now_ts)

    # Get VRAM at end
    vram_end=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader | tr -d ' ' | sed 's/MiB//')

    # Calculate load time
    LOAD_TIME=$(python3 -c "print(f'{$END - $START:.3f}')")
    echo "  Total time (request -> response): ${LOAD_TIME}s"
    echo "  VRAM at end: ${vram_end} MiB"

    # Record result
    echo "${run},${LOAD_TIME},${vram_end}" >> "$RESULTS_FILE"

    # Kill sampler
    kill $SAMPLER_PID 2>/dev/null || true
    wait $SAMPLER_PID 2>/dev/null || true

    # Stop model for next cold start
    if [ "$run" -lt "$RUNS" ]; then
        ollama stop "$MODEL" 2>/dev/null || true
        sleep 3
    fi
done

###############################################################################
# Analyze GPU timeline from last run
###############################################################################
echo ""
echo "============================================"
echo "           GPU TIMELINE (Last Run)"
echo "============================================"

LAST_TIMELINE="$OUTPUT_DIR/timeline_run_${RUNS}.csv"
if [ -f "$LAST_TIMELINE" ]; then
    awk -F',' '
    NR == 1 { next }
    {
        t = $1 + 0;
        power = $2 + 0;
        util = $4 + 0;
        mem = $5 + 0;

        if (mem < 5000) phase = "BASELINE";
        else if (util > 20) {
            if (mem < 15000) phase = "LOADING";
            else phase = "GENERATING";
        }
        else phase = "IDLE_LOADED";

        if (phase != prev_phase) {
            printf "  t=%6.1fs | %-13s | VRAM=%6d MiB | GPU=%3d%% | Power=%5.1fW\n",
                   t, phase, mem, util, power;
            phase_start[phase] = t;
        }
        phase_end[phase] = t;
        prev_phase = phase;
    }
    END {
        printf "\n";
        if ("LOADING" in phase_start && "IDLE_LOADED" in phase_start) {
            load_time = phase_end["IDLE_LOADED"] - phase_start["LOADING"];
            printf "  Estimated pure load time (disk -> VRAM): %.1fs\n", load_time;
        }
    }
    ' "$LAST_TIMELINE"
fi

###############################################################################
# Summary statistics
###############################################################################
echo ""
echo "============================================"
echo "           LOAD TIME SUMMARY"
echo "============================================"
echo ""

awk -F',' '
NR == 1 { next }
{
    times[NR-1] = $2 + 0;
    sum += $2;
    count++;
    if (count == 1 || $2 > max) max = $2;
    if (count == 1 || $2 < min) min = $2;
}
END {
    mean = sum / count;
    for (i in times) {
        variance += (times[i] - mean)^2;
    }
    stddev = sqrt(variance / count);

    printf "  Runs completed:    %d\n", count;
    printf "  Mean total time:   %.1f s\n", mean;
    printf "  Std deviation:     %.2f s\n", stddev;
    printf "  Min:               %.1f s\n", min;
    printf "  Max:               %.1f s\n", max;
    printf "\n";
    printf "  Context for comparison:\n";
    printf "  - Acceptable voice response latency:  < 1.0 s\n";
    printf "  - Noticeable delay threshold:          ~2.0 s\n";
    printf "  - Human conversation pause:            0.5-2.0 s\n";
    printf "  - Model load is %.0fx a noticeable delay\n", mean / 2.0;
    printf "  - Model load is %.0fx acceptable voice latency\n", mean / 1.0;
}
' "$RESULTS_FILE"

echo ""
echo "=== Raw data: $RESULTS_FILE ==="
