# Benchmark: 7B instruct model for `llm.small` (EVB-23)

**Date:** 2026-11-10
**Machine:** RTX 4090 24564 MiB (driver 595.99.02), Ryzen (16 cores/threads)
**Engine:** llama.cpp 0.7.0-dev (build 1, commit 7170cad4d) — the pinned
brain build, `/opt/jarvis-engine/bin/llama-server`, run in a throwaway
container (`jarvis/engine:local`, `--gpus all`, host network, port 11530).

The `llm.small` capability worker runs a 7-8B instruct model as its own
llama-server coprocess for batch sub-tasks (summarise / classify /
extract over files), on the 4090's **spare** VRAM (GPU placement) or in
host RAM (CPU fallback). This doc records the model choice and the VRAM
measurements behind the `capabilities.yaml` entry and the GPU lease.

## Model choice: Qwen2.5-7B-Instruct Q4_K_M

The job spec names "Qwen2.5-7B-Instruct-class or similar; record the
choice". Qwen2.5-7B-Instruct is exactly that class — the stock 7B
instruct model, same family as the iGPU helper (Qwen2.5-3B), so its
instruction-following behaviour is already known on this machine.

**Sourcing:** huggingface.co is IP-blocked from this box (401 on every
path, no HF token present), so the official Qwen GGUF was pulled from
ModelScope (`Qwen/Qwen2.5-7B-Instruct-GGUF`, the same official repo
family) with the standard `scripts/docker-fetch.sh`:

| file | bytes | MiB |
| --- | --- | --- |
| `qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf` | 3 993 201 344 | 3808.4 |
| `qwen2.5-7b-instruct-q4_k_m-00002-of-00002.gguf` | 689 872 288 | 657.9 |
| **total** | 4 683 073 632 | **4465.9** |

Kept at `/opt/jarvis-llm-small/models/` (compose mounts it read-only).
The pinned llama-server loads the split file when given the first part —
verified empirically in the bench (the `-00002` sibling loaded itself).

## Method

- The bench container runs the host's pinned `llama-server` directly on
  the model, `--ctx-size 8192` (the worker's `LMS_CTX`), one short chat
  completion per configuration (`max_tokens 50`, `temperature 0.2`).
- **VRAM:** `nvidia-smi` used-memory before load / after load / peak
  after inference. The brain was loaded and idle throughout (this
  benchmark was produced from the brain's own round — the brain was
  never touched); the numbers are deltas, so the brain's own ~20 GB
  does not contaminate them.
- GPU runs used `--n-gpu-layers 16` and `28`; CPU runs used
  `--n-gpu-layers 0 --threads 16`.

## Results

### GPU (partial load, 16/28 layers)

| | MiB |
| --- | --- |
| used before | 20 866 (brain idle) |
| used after load (16/28) | 24 205 → **delta 3339** |
| peak after inference | 24 199 (3333) |
| weights @ 16 layers (llama-server log) | 2551.9 |
| KV cache + ctx 8192 (remainder) | ~787 |

16 layers ≈ 2552 MiB → 159.5 MiB per layer → 28 layers ≈ **4466 MiB of
weights** (matches the file total exactly) → full footprint
≈ 4466 + 787 (KV) + overhead ≈ **5.3–5.5 GB**. The 32-token completion
took 0.2 s; VRAM fully freed after the server exited.

**Recorded in the catalog:** `vram_gb: 5.4` (models.yaml), `vram_mb:
5500` (capabilities.yaml).

### GPU lease — the co-scheduling math

The plan (§4, Phase 3) co-schedules small workers with image jobs:
the brain is out for the 12 GB image job, leaving ~10 GB of the
24564 MiB free. With the brain **idle** (its normal state) the free
VRAM is ~3.7 GB — a 5.4 GB load would not fit, and the brain must
never be stopped for this. So the GPU placement is lease-gated:

```
LMS_MIN_FREE_MB = 6656  (6.5 GB = 5.4 GB footprint + headroom)
```

- free ≥ 6.5 GB (the image window) → lease grants, 7B loads on the GPU
- free < 6.5 GB (idle/conversational brain) → **session refused**, the
  message stays available and the CPU placement (consumer priority 1)
  takes it

The refusal is a routing decision, never an OOM: the coprocess is only
started after the lease grants (capworker contract — a Lease error
means the session is not granted). Full in-window GPU verification
(loading the 7B while the image job holds 12 GB) is the plan's EVB-28
job, which records any correction back into `capabilities.yaml`.

### CPU (0/28 layers, 16 threads)

| | |
| --- | --- |
| residual VRAM floor | **612–658 MiB** (the binary always inits the CUDA backend; `--n-gpu-layers 0`) |
| RSS at rest (docker stats) | 681.5 MiB — the weights are mmap-backed page cache, not resident RSS |
| decode speed | 50 tokens in 22.4 s ≈ **2.2 tok/s** (consistent with the EVB-21 CPU bench: 2.2 tok/s on 8B) |
| correctness | clean, on-task reply |

**Recorded:** `vram_mb: 658`, `ram_mb: 5120` (weights + KV + buffers;
the mmap makes the real pressure lower, but the catalog budgets the
worst case). `typical_runtime: 240s` — at ~2 tok/s a few hundred tokens
of reply is minutes. The CPU placement is the fallback, not the fast
path: it exists so a sub-task is never blocked on the window.

## Boundary (documented per the job spec)

The iGPU helper (Qwen2.5-3B on :11435) remains the **conversation**
fallback during the brain's swap windows. `llm.small` is for **batch
sub-tasks only** — one-shot structured calls over files, no
multi-turn dialogue. The note lives in `config/capabilities.yaml`
(model-facing) and in the worker's package doc.
