# Wake-to-First-Word Latency Measurements

**Date:** 2026-10-21
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM), Intel CPU
**STT Model:** faster-whisper base.en (CPU int8 for baseline, GPU float16 for production)
**LLM Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama
**TTS Engine:** espeak-ng
**Runs per stage:** 5

## Motivation

Following the model load time and GPU power measurements, we now measure the
latency of each stage in the wake-to-response pipeline to determine if the
system can meet the 1-3 second target for natural voice interaction.

## Methodology

1. **STT (Speech-to-Text):** Generated test audio with espeak-ng, measured
   transcription time with faster-whisper on both CPU and GPU.
2. **LLM First Token:** Sent minimal prompt to Ollama with thinking disabled,
   measured time to first token in the streaming response.
3. **TTS (Text-to-Speech):** Measured time from text input to first audio byte
   written using espeak-ng.
4. Each stage measured 5 times; statistics computed over all runs.

## Results

### Stage 1: Speech-to-Text (faster-whisper base.en)

| Metric | CPU (int8) | GPU (float16) |
|--------|-----------|---------------|
| Mean | 0.315 s | 0.087 s |
| Std Dev | 0.035 s | 0.075 s |
| Min | 0.297 s | 0.034 s |
| Max | 0.377 s | 0.219 s |
| P50 | 0.3 s | 0.055 s |
| Model Load (CPU) | 0.604 s | — |

**STT device used for pipeline calc:** cuda

### Stage 2: LLM Time to First Output (jarvis-brain)

Note: The jarvis-brain model (qwen3.6:27b) always outputs thinking tokens, even when
"think": false is set. "First output" measures when the first token arrives (whether
in the response or thinking field). "Prompt eval" measures how long the model spent
processing the input prompt before generating any output.

A separate warmup run was performed before the 5 measured runs to account
for cold KV cache initialization. Warmup time: 5.020s (not included in stats).

| Metric | First Output (s) | Prompt Eval (s) | Total Response (s) |
|--------|------------------|-----------------|--------------------|
| Mean | 0.113 | 0.111 | 0.201 |
| Std Dev | 0.0 | 0.0 | 0.002 |
| Min | 0.113 | 0.111 | 0.199 |
| Max | 0.114 | 0.112 | 0.203 |
| P50 | 0.113 | 0.111 | 0.2 |

### Stage 3: Text-to-Speech (espeak-ng)

| Metric | First Byte | Total |
|--------|-----------|-------|
| Mean | 0.006 s | 0.006 s |
| Std Dev | 0.0 s | 0.0 s |
| Min | 0.006 s | 0.006 s |
| Max | 0.007 s | 0.007 s |
| P50 | 0.006 s | 0.006 s |

### Pipeline Summary

| Metric | Value |
|--------|-------|
| Mean Pipeline Latency | 0.206 s |
| P50 Pipeline Latency | 0.174 s |
| Target | 1-3 s |
| **Within Target?** | YES |

## Per-Run Data

### STT (GPU) Runs

| Run | Transcribe (s) |
|-----|----------------|
| 1 | 0.219 |
| 2 | 0.034 |
| 3 | 0.055 |
| 4 | 0.051 |
| 5 | 0.074 |

### LLM Runs

| Run | First Output (s) | Prompt Eval (s) | Total (s) | Tokens |
|-----|------------------|-----------------|-----------|--------|
| 1 | 0.113 | 0.111 | 0.2 | 8 |
| 2 | 0.113 | 0.111 | 0.199 | 8 |
| 3 | 0.113 | 0.111 | 0.2 | 8 |
| 4 | 0.113 | 0.111 | 0.202 | 8 |
| 5 | 0.114 | 0.112 | 0.203 | 8 |

### TTS Runs

| Run | First Byte (s) | Total (s) |
|-----|----------------|-----------|
| 1 | 0.007 | 0.007 |
| 2 | 0.006 | 0.006 |
| 3 | 0.006 | 0.006 |
| 4 | 0.006 | 0.006 |
| 5 | 0.006 | 0.006 |

## Analysis

### Breaking Down the Pipeline

The wake-to-first-word pipeline has three sequential stages:

1. **Wake word detected** -> **STT transcribes** -> **LLM generates** -> **TTS speaks**

The critical path is the sum of STT transcription, LLM first token, and TTS
first byte. These run sequentially in the simplest implementation.

### Where the Time Goes

- **STT (0.087s mean):** faster-whisper base.en is fast enough on GPU.
  The model is small (~150MB) and can be pre-loaded.
- **LLM first output (0.113s mean, prompt eval 0.111s):** This is the dominant stage.
  The 27B model needs time to process the prompt. Note: this model always outputs
  thinking tokens regardless of API settings.
- **TTS (0.006s mean):** espeak-ng is very fast; negligible contribution.

### Can We Meet the Target?

**Yes.** The mean pipeline latency of 0.206s is within the 1-3s target.

## Raw Data

Measurement script: `scripts/measure_wake_latency.py`
