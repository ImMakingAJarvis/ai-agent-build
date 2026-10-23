# Qwen 3.8 Next Flash MoE 128B Benchmark

**Date:** 2026-10-23 06:23:06 UTC
**Job:** chat-1792733622-6ba1
**Target Model:** qwen3.8-flash-next (MoE ~128B parameters)

## System Information

- **GPU:** NVIDIA GeForce RTX 4090, 24564 MiB, 595.99.02
- **Ollama:** ollama version is 0.40.7
- **VRAM Used:** 23915 MiB / 24564 MiB (97.4% used, 0.6 GB free)
- **Loaded Models:**
  - NAME                   ID              SIZE     PROCESSOR    CONTEXT    RUNNER      UNTIL
  - jarvis-brain:latest    9b13c34f873a    17 GB    100% GPU     65536      llamacpp    Forever

## Model Availability Check

### Ollama Library Tags

The only available tag for `qwen3.8-flash-next` is:

- `qwen3.8-flash-next:125b-mlx` — 105 GB, MLX format (Apple Silicon only)

This model is **cloud-only** on Ollama. Local pull requires Ollama cloud signin,
and the MLX format is incompatible with Linux/CUDA.

### Pull Attempt

```bash
$ ollama pull qwen3.8-flash-next
  [?2026h[?25l[1Gpulling manifest ⠋ [K[?25h[?2026l[?2026h[?25l[1Gpulling manifest ⠙ [K[?25h[?2026l[?2026h[?25l[1Gpulling manifest ⠹ [K[?25h[?2026l[?2026h[?25l[1Gpulling manifest [K[?25h[?2026l
  Error: pull model manifest: file does not exist
```

**Pull result:** Failed (rc=1)

## VRAM Feasibility Analysis

| Parameter | Value |
|---|---|
| GPU VRAM | 24.0 GB |
| Available VRAM | 0.6 GB |
| Model parameters | ~128B (MoE) |
| Q4 quantized size (estimated) | ~64 GB |
| Q2 quantized size (estimated) | ~32 GB |
| MLX tag size | 105 GB |

**Conclusion:** Even at the most aggressive quantization (Q2), a 128B model
requires ~32 GB VRAM, exceeding the RTX 4090's 24.0 GB by 8.0 GB.
The MoE architecture activates a subset of parameters per token, but Ollama's
llamacpp runner still needs to load the full weight file into memory.

## Comparison with Existing Local Models

| Model | Params | Type | VRAM | Gen Speed |
|---|---|---|---|---|
| qwen3.8-flash-next | ~128B | MoE | ~64 GB (Q4) | N/A - cannot load |
| qwen3:30b-a3b (jarvis-moe) | 30B | MoE | ~23 GB | 212 tok/s |
| qwen3.6:27b (jarvis-brain) | 27B | Dense | ~17 GB | 103 tok/s |

## Benchmark Attempt

### Benchmark Skipped

Benchmark could not run because the model could not be pulled.

**Error:** Model `qwen3.8-flash-next` is cloud-only and requires Ollama cloud signin.
The only available local tag (`125b-mlx`) is Apple Silicon format.

## Cleanup

Attempting to unload and remove the model...
- Stop: failed (model not loaded)
- Remove: failed (model not present)

**Final VRAM:** 23915 MiB / 24564 MiB

## Summary

| Check | Result |
|---|---|
| Model available locally | ❌ Cloud-only / MLX-only |
| Pull successful | ❌ No compatible tag for Linux/CUDA |
| VRAM sufficient | ❌ 128B model needs ~64 GB, RTX 4090 has 24.5 GB |
| Benchmark completed | ❌ Skipped - model unavailable |

### Recommendation

The Qwen 3.8 Next Flash MoE 128B model **cannot be benchmarked locally** on the
RTX 4090 due to:

1. **Cloud-only availability:** The model requires Ollama cloud signin.
2. **VRAM insufficient:** Even at Q4 quantization (~64 GB), the model exceeds
   the RTX 4090's 24.5 GB VRAM by nearly 3x.
3. **No compatible format:** The only visible tag is MLX (Apple Silicon).

To benchmark this model, options are:
- Use Ollama cloud inference (requires signin)
- Use a multi-GPU system or cloud GPU with 80+ GB VRAM
- Benchmark a smaller MoE model as a proxy (e.g., qwen3:30b-a3b already benchmarked)

