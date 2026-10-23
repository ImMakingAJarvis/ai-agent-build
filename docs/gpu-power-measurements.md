# GPU Power Measurements: Idle Model vs Generation

**Date:** 2026-10-21
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM, 450W power limit)
**Model:** qwen3.6:27b Q4_K_M (~17 GB weights, ~23.4 GB VRAM when loaded)
**Tool:** Ollama 0.40.7

## Motivation

Deciding whether the `jarvis-brain` model should stay loaded in VRAM at all times, or be loaded on-demand. A related question: is a separate tiny always-on model worth the extra complexity?

## Methodology

Used `nvidia-smi` to sample GPU power draw, temperature, utilization, and VRAM every 1 second. Sent API requests to Ollama to trigger model loading, idle periods, and active generation. Categorized samples by VRAM usage and GPU utilization.

**Note:** `OLLAMA_KEEP_ALIVE=-1` is configured in systemd but does NOT prevent the model from unloading after API responses. The model unloads within seconds of a response completing, regardless of `keep_alive` settings. This made extended idle measurements difficult.

## Results

### GPU Baseline (No Model Loaded)

| Metric | Value |
|--------|-------|
| Samples | 36 |
| Average Power | **57.8 W** |
| Stable Idle | 59-60 W |
| Min | 30.5 W |

### Model Loaded, Idle (Not Generating)

The model was fully loaded into VRAM (23,347-23,395 MiB) with 0% GPU utilization. These samples were captured during the brief window between load completion and generation start.

| Metric | Value |
|--------|-------|
| Samples | 12 |
| Average Power | **71.4 W** |
| Range | 59.0 - 88.8 W |
| Typical | 60-78 W |

### Model Generating (Active Token Production)

| Metric | Value |
|--------|-------|
| Samples | 81 (~16 seconds) |
| Average Power | **411.9 W** |
| Range | 367.0 - 416.1 W |
| GPU Utilization | 92-98% |

## Analysis

### Power Differential: Idle Model vs Baseline

Keeping the 27B model warm in VRAM costs **~14 W extra** (71.4 W - 57.8 W).

| Scenario | Power | Extra vs Baseline |
|----------|-------|-------------------|
| GPU idle, no model | 57.8 W | — |
| GPU idle, model in VRAM | 71.4 W | +13.6 W |
| GPU generating | 411.9 W | +354.1 W |

### Cost Analysis

At $0.12/kWh:
- **Keeping model warm 24/7:** 13.6 W x 24h x 365d = 119 kWh/year = **$14.30/year** (~4 cents/day)
- **Loading on-demand:** ~9 seconds to load, ~16 seconds to generate a long response

### The Tiny Model Question

A separate tiny always-on model (e.g., 1-3B parameters) would:
- Consume 2-6 GB VRAM and ~20-30 W continuously
- Still require loading the 27B model for any real task (9-second delay)
- Add operational complexity (two models to manage, monitor, update)
- Provide no meaningful power savings over just keeping the 27B model warm

## Conclusion

**No tiny always-on model is needed.** The power cost of keeping `jarvis-brain` warm is negligible (~14 W, ~$14/year).

**Recommendation (updated by load-time measurements):** Keep `jarvis-brain` resident in VRAM at all times. Cold-start load time is ~8.6 seconds — far too slow for post-wake-word interaction (see `docs/model-load-time-measurements.md`). The 14 W idle overhead is acceptable. Use `OLLAMA_KEEP_ALIVE` or a systemd timer to prevent unloading.

## Raw Data

Measurement script: `scripts/measure_gpu_power.sh`
Raw CSV data: `/tmp/gpu_power_measurements/fast_combined.csv`
