# MoE 128K Context Suite Plan

## Purpose

This plan defines the benchmark suite used to find the optimal MoE expert-cache
size for the 128K-context MoE models:

- Qwen 3.8 Flash Next (MoE), UD-IQ4_XS
- GLM 5.3 Flash (MoE), UD-IQ2_XXS

The suite fixes the context window at **131072 tokens** and sweeps a VRAM
ladder. For each ladder point, the engine resolves an expert-cache size from
the remaining VRAM after the model weights, the 131072-token KV reservation,
runtime overhead, and the target free-VRAM headroom.

The goal is to record, for each model:

1. The resolved `moe_cache_size` at each VRAM ladder point.
2. The lowest free-VRAM point at which the full suite completes.
3. The optimal expert-cache size, defined as the largest expert-cache size that
   still completes the full 131072-token suite without OOM, truncation, or
   suite failure.
4. Evidence for whether the model is a good enough coder to install as
   thinker/planner/coder.

## Models

| Model | Quant | Path |
|---|---|---|
| Qwen 3.8 Flash Next (MoE) | UD-IQ4_XS | `/home/agent/models/qwen3.8-flash-next/UD-IQ4_XS` |
| GLM 5.3 Flash (MoE) | UD-IQ2_XXS | `/home/agent/models/glm5.3-flash/UD-IQ2_XXS` |

The benchmark uses the first GGUF shard of each model as the model path for
`ft serve`.

## Fixed 131072-token context

The suite is invalid unless the engine reports an effective context of at least
131072 tokens.

- `max_seq_len`: 131072
- KV reservation: 131072 tokens
- Truncation: failure
- Context shorter than 131072: failure
- OOM during any battery component: failure for that ladder point

## 131072-token VRAM ladder

The ladder fixes the sequence length and KV reservation at **131072 tokens**.
It sweeps target free-VRAM headroom rather than changing the context length.

The planning formula is:

`moe_cache_bytes = vram_total - vram_weights - vram_kv_131072 - vram_runtime_overhead - target_free_vram`

`moe_cache_size = floor(moe_cache_bytes / moe_cache_bytes_per_token)`

The resolved `moe_cache_size` is capped at 131072. If the calculated value is
negative, the ladder point is recorded as infeasible with `moe_cache_size=0`.

### Ladder points

| Point | Target free VRAM | Intent |
|---|---:|---|
| H8 | 8 GiB | Conservative; should fit if the model is viable at 131072 tokens |
| H6 | 6 GiB | Moderate headroom |
| H4 | 4 GiB | Aggressive headroom |
| H2 | 2 GiB | Maximum practical cache size before OOM risk becomes high |

### Expert-cache size to VRAM overhead mapping

The planning estimate used by the suite is 1 GiB per 32768 cached tokens.
The runner records the actual engine-reported overhead when available.

| `moe_cache_size` | Planning VRAM overhead |
|---:|---:|
| 0 | 0 GiB |
| 16384 | 0.5 GiB |
| 32768 | 1.0 GiB |
| 65536 | 2.0 GiB |
| 131072 | 4.0 GiB |

### Expected resolved sizes on a 24 GiB host

The values below are planning estimates for a 24 GiB GPU. The actual run must
record the engine-resolved values.

| Model | Weights estimate | KV 131072 estimate | Runtime overhead estimate | Residual before headroom |
|---|---:|---:|---:|---:|
| Qwen 3.8 Flash Next (MoE) | 14.0 GiB | 3.5 GiB | 1.0 GiB | 5.5 GiB |
| GLM 5.3 Flash (MoE) | 10.5 GiB | 3.5 GiB | 1.5 GiB | 8.5 GiB |

| Model | H8 | H6 | H4 | H2 |
|---|---:|---:|---:|---:|
| Qwen 3.8 Flash Next (MoE) | 0 | 0 | 32768 | 65536 |
| GLM 5.3 Flash (MoE) | 16384 | 65536 | 131072 | 131072 |

## Expected optimal cache size

The optimal cache size is the largest `moe_cache_size` at which the full
131072-token test battery completes without OOM, truncation, timeout, or
incorrect required output.

Expected results on a 24 GiB host:

| Model | Expected optimal `moe_cache_size` | Fallback if optimal point fails |
|---|---:|---:|
| Qwen 3.8 Flash Next (MoE) | 65536 | 32768 |
| GLM 5.3 Flash (MoE) | 131072 | 65536 |

If the host has more than 24 GiB of usable VRAM, the expected optimal size for
both models is 131072, provided the full battery completes.

## Test battery

The test battery has three components. A ladder point passes only when all
three components pass.

### 1. Standard 3-run prompt

Purpose: measure stability, warmup behavior, and baseline latency at the full
131072-token context.

- Prompt: `You are given a 131072-token context. Summarize it in exactly three bullets. Then answer: What is the project name?`
- Context: a 131072-token scaffold containing the project name `moe-128k-suite`.
- Runs: 3 consecutive runs.
- Sampling: seed 1337, temperature 0, top_p 1.
- Max output tokens: 256.
- Pass criteria:
  - All 3 runs complete.
  - No truncation.
  - Each response contains `moe-128k-suite`.
  - Latency and token counts are recorded for all 3 runs.

### 2. Coding battery

Purpose: determine whether the model is a good enough coder to install as
thinker/planner/coder under the 131072-token context.

Decision for the open question: the coding battery reuses existing prompts
from `bench/prompts.yaml`. It does not require new 128K-specific coding
prompts. It does require 128K-specific context scaffolding around those
existing prompts.

- Prompt source: `bench/prompts.yaml` in the active worktree.
- If `bench/prompts.yaml` is missing, the suite fails with
  `missing-coding-prompts`.
- The runner selects the first four available coding prompts.
- Each coding prompt is wrapped in a 131072-token context containing
  repository files, logs, documentation, and related code excerpts.
- The original coding instruction remains the task; the surrounding context is
  the 128K scaffold.
- Pass criteria:
  - All selected coding prompts complete.
  - No truncation.
  - Each response contains the required answer, patch, or file edit marker.
  - If a prompt has deterministic unit tests, those tests must pass.
  - If a prompt has no tests, the runner uses the prompt's expected-output
    checker.

### 3. 128K needle test

Purpose: verify that the model can retrieve a unique token from a full
131072-token context.

- Context length: 131072 tokens.
- Filler: neutral documentation or code text with no competing needle-like
  identifiers.
- Needle positions: 10%, 50%, and 90% of the context.
- Needle format: `needle-id: moe128k-<model-slug>-<position>-<run-id>`
- Question: `Return the exact needle-id.`
- Runs: one run per needle position.
- Pass criteria:
  - All three needle positions complete.
  - No truncation.
  - Each response contains the exact needle-id for that position.
  - No position may be answered by guessing a neighboring position.

## Execution order

1. Preflight:
   - Verify model path exists.
   - Verify the first GGUF shard is present.
   - Verify `max_seq_len` is 131072.
   - Verify usable VRAM.
   - Verify `bench/prompts.yaml` exists for the coding battery.
2. For each model:
   - Run ladder points in order: H8, H6, H4, H2.
   - For each ladder point:
     - Resolve `moe_cache_size`.
     - Start the engine with the resolved cache size.
     - Run the standard 3-run prompt.
     - Run the coding battery.
     - Run the 128K needle test.
     - Record pass/fail, latency, VRAM, and failure reason.
3. After all ladder points:
   - Select the optimal `moe_cache_size` as the largest passing point.
   - Record the lowest free-VRAM point that still passes.
   - Record whether the model is suitable as thinker/planner/coder.

## Results and failure criteria

Each ladder point produces one result row.

| Field | Meaning |
|---|---|
| `model` | Model name and quant |
| `ladder_point` | H8, H6, H4, or H2 |
| `target_free_vram` | Target free VRAM in GiB |
| `resolved_moe_cache_size` | Engine-resolved expert-cache size |
| `standard_3_run` | Pass or fail |
| `coding_battery` | Pass or fail |
| `needle_test` | Pass or fail |
| `overall` | Pass only if all three components pass |
| `failure_reason` | `oom`, `truncation`, `timeout`, `wrong-answer`, `missing-prompt`, or empty |

A ladder point is a failure if any component fails. The optimal cache size is
never inferred from a partial pass.

## Open questions resolved

- Coding battery definition: reuse existing prompts from `bench/prompts.yaml`;
  do not create new 128K-specific coding prompts for this suite. Add only the
  128K context scaffold required to make each prompt run at 131072 tokens.
- GLM 5.3 Flash model path: use
  `/home/agent/models/glm5.3-flash/UD-IQ2_XXS` and the first GGUF shard for
  `ft serve`. No separate KV-cache format is required beyond the standard
  131072-token reservation.
