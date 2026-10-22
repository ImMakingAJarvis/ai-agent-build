# Model Load Time Measurement: NVMe Cold Start

**Date:** 2026-10-21
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM), NVMe SSD
**Model:** qwen3.6:27b Q4_K_M (~17 GB weights, jarvis-brain)
**Tool:** Ollama 0.40.7

## Motivation

Determine whether loading `jarvis-brain` from NVMe after a wake-word detection is fast enough for natural voice interaction, or if the model must stay resident in VRAM at all times.

## Methodology

1. Stopped the model to ensure a cold start (VRAM < 2 GB)
2. Sent a minimal chat request to trigger the load
3. Measured total time from request to response received
4. Sampled GPU power, temperature, utilization, and VRAM every 200ms during load
5. Repeated 3 times with full unload between runs

Measurement script: `scripts/measure_model_load_time.sh`

## Results

### Cold Start Load Times

| Run | Total Time (request → response) | VRAM at End |
|-----|--------------------------------|-------------|
| 1   | 8.45 s                         | 23,455 MiB  |
| 2   | 8.78 s                         | 23,429 MiB  |
| 3   | 8.58 s                         | 23,429 MiB  |
| **Mean** | **8.6 s**                  | —           |
| Std Dev | ±0.14 s                    | —           |

### GPU Activity Timeline (Run 3)

| Time | Phase | VRAM | GPU | Power | Event |
|------|-------|------|-----|-------|-------|
| 0.0s | Baseline | 1,172 MiB | 2% | 20.4W | Model unloaded |
| 0-2s | Prefetch | 1,169-2,163 MiB | 2% | 20-59W | Page cache / read ahead |
| 3.0s | **Weights loaded** | 16,928 MiB | 2% | 57.3W | Model in VRAM |
| 3-4s | Settling | 16,928-16,933 MiB | 2% | 57-75W | Model idle in VRAM |
| 5.0s | KV cache alloc | 23,411 MiB | 0% | 71-76W | Context pre-allocated |
| 6.0s | Ramp up | 23,431 MiB | 4% | 96-209W | GPU warming |
| 7.0s | **Generation** | 23,431 MiB | 96% | 361-403W | Token production |
| 8.5s | Done | 23,429 MiB | — | — | Response returned |

### Breakdown

| Phase | Duration | Description |
|-------|----------|-------------|
| Disk → VRAM (weights) | **~3 seconds** | NVMe read, PCIe transfer to GPU memory |
| VRAM settle + KV cache | **~2 seconds** | Context window pre-allocation |
| Generation (first response) | **~1.5 seconds** | "OK" token production |
| **Total cold start → response** | **~8.6 seconds** | |
| **Cold start → ready for input** | **~5 seconds** | Model loaded + context ready |

## Analysis

### Is 3 Seconds (Load) / 8.6 Seconds (Full) Acceptable After a Wake Word?

| Benchmark | Latency | Model Load |
|-----------|---------|------------|
| Acceptable voice response | < 1.0 s | **9× slower** |
| Noticeable delay threshold | ~2.0 s | **4× slower** |
| Human conversation pause | 0.5-2.0 s | **4-17× slower** |
| Alexa/Siri response target | < 1.5 s | **6× slower** |

An 8.6-second delay after a wake word would feel like the system is broken, not slow. Even the 3-second pure load time is 3× the "noticeable delay" threshold.

### Why On-Demand Loading Doesn't Work for Voice

After a wake word, the user expects an immediate acknowledgment ("Yes?" or a listening tone). An 8.6-second silent delay before any response:
- Breaks the illusion of a listening assistant
- Feels like a crash or network failure
- Is 4-9× slower than commercial voice assistants

### Power Cost of Keeping Model Resident

From the GPU power measurements (`docs/gpu-power-measurements.md`):
- Idle with model in VRAM: **71.4 W** (vs. 57.8 W baseline = **+14 W**)
- Annual cost at $0.12/kWh: **~$14.70/year** (~4 cents/day)

## Conclusion

**The model must stay resident in VRAM.** Loading from NVMe takes ~3 seconds for weights alone, and ~5 seconds until the model is ready to process input. Total cold start to first response is ~8.6 seconds — far too slow for post-wake-word interaction.

The power cost of keeping it warm is negligible (~$14/year). There is no benefit to a separate tiny always-on model (see `docs/gpu-power-measurements.md` for analysis).

**Recommendation:** Keep `jarvis-brain` loaded in VRAM at all times. Use `OLLAMA_KEEP_ALIVE` or a systemd timer to prevent unloading. The 14 W idle overhead is acceptable.

## Raw Data

- Measurement script: `scripts/measure_model_load_time.sh`
- Load times CSV: `/tmp/model_load_time/load_times.csv`
- GPU timeline (run 3): `/tmp/model_load_time/timeline_run_3.csv`
