# Helper model options — two helpers (CPU + iGPU)?

Job chat-1793249102-d283. Nathan's question: would two helpers — one
on the CPU, one on the iGPU — be useful, and what other models could
we put on this machine? Follow-up to the 3B CPU-vs-iGPU benchmark
(`docs/igpu-model-handler.md`; the 3B rows and the broken-CPU-pp
footnote land in PR #109).

**TL;DR — yes, a second helper is worth it, but not a second copy of
the 3B, and not on the CPU.** A duplicate 3B buys nothing: the iGPU 3B
already does its job at 13.5 tok/s, and a CPU copy is only 1.7× faster
while burning 16 cores. The useful second helper is a *bigger* model —
**Qwen3-30B-A3B (MoE, 17.3 GB)**: 30B-class quality at ~3B decode
cost, because only ~3.3B params are active per token. The new bench in
this round found that **for MoE the iGPU beats the CPU at decode**
(11.26 vs 6.24 tok/s), reversing the dense-model result where the CPU
wins (22.9 vs 13.6). So the tier is: 3B on the iGPU always-on
(`jarvis-engine-igpu`), 30B-A3B on the same iGPU *on demand* — and
the CPU stays free for the voice pipeline, the memory service, and the
~128B FreeToken model (which needs CPU + PCIe bandwidth anyway). Other
candidates: a 1B router, embedding/reranker upgrades for the memory
service, Whisper STT, a small vision model, and the already-downloaded
~128B MoE via FreeToken as a *swappable brain* (it cannot run
alongside the current brain).

## Current handlers

| Handler | Model | Where | Speed (measured) | Role |
|---|---|---|---|---|
| `jarvis-engine-igpu` | Qwen2.5-3B Q4_K_M (2 GB) | iGPU Vulkan1, port 11435 | 13.55 tg / 116.7 pp tok/s | always-on helper, zero CPU cost |
| `jarvis-engine` (brain) | qwen3.8 27B Q4_K_M (16 GB) | 4090, port 11434 | ~48–103 tg tok/s | the agent itself |
| CPU | — | 7800X3D, 16 threads, 128 GB RAM | 22.94 tg tok/s (3B) | currently unused for models |

RAM: 122 GiB total, ~59 GiB free at idle (2026-10-29). The 4090 is
full (18.5 GB of 24 GB under the brain), so the 4090 has no spare
capacity — new models go on CPU or iGPU, or replace the brain.

## Why not two copies of the 3B

Measured (PR #109): CPU decode 22.94 vs iGPU 13.55 tok/s (1.69×), iGPU
prefill 116.7 vs CPU 45–49 tok/s (2.2×). Running the same 3B on both
devices gives two helpers of *equal intelligence*; the faster one
(CPU) costs 16 busy cores while decoding, which is exactly what the
iGPU helper avoids. The iGPU 3B's 13.5 tok/s is already fine for its
job (10-line setup answers), and its prefill edge is the right fit for
reading the setup brief + live snapshot on every call. **Keep one 3B,
on the iGPU.** If it ever feels slow, moving it to the CPU is one line
in the unit file — a speed tweak, not a new capability.

## The second helper: Qwen3-30B-A3B on the iGPU (on demand)

MoE decode is bandwidth-bound on the *active* weights, not the total.
30B-A3B activates ~3.3B params per token (~2 GB of Q4 weights touched
per step), so it decodes at roughly the bandwidth a 2–3 GB dense model
would need; the other ~15 GB sits dormant in RAM. The same model
measured **2× the brain's throughput on the 4090** (212 vs 103 tok/s,
`docs/moe-benchmark.md`), where the 4090 is ~19× faster than the
iGPU but has no free VRAM.

Measured 2026-10-29, `llama-bench` pp512/tg512, 3 repeats (+ the
default warmup run), `qwen3:30b-a3b` Q4 from the Ollama store
(17.3 GB blob, already on disk; 3B helper idle during the run):

| device | pp512 tok/s | tg512 tok/s |
|---|---|---|
| iGPU (Vulkan1) | 54.09 ± 0.42 | 11.26 ± 0.01 |
| CPU (16 threads) | 69.59 ± 3.19 | 6.24 ± 4.97 |

**The decode result reverses the dense benchmark.** Dense 3B: CPU
22.94 vs iGPU 13.55 (CPU 1.69× — same ~2 GB read per token, and the
CPU's ~45 GB/s beats the iGPU's ~26 GB/s). MoE 30B-A3B: iGPU 11.26 vs
CPU 6.24 (iGPU 1.8×), with the iGPU also far steadier (±0.01 vs
±4.97 — the noisy CPU run likely hit background load). Same ~2 GB
active-weights read per token, so the CPU MoE path in this build is
simply much less efficient (small per-expert GEMVs, routing overhead,
and a 17 GB working set that thrashes the cache).

Prefill flips the other way from the dense case: CPU 69.6 vs iGPU
54.1 (CPU 1.28×) — the iGPU's 2.2× dense-prefill edge (PR #109) does
not carry over to MoE in this build. The CPU pp row passes the TFLOPS
sanity check (~0.5 TFLOPS effective vs ~1.2 peak), so unlike the
broken dense CPU-pp numbers it is trustworthy.

For the helper workload (moderate prompt, 100–500 token answer)
decode dominates: a 1K-token prompt + 300-token answer takes ≈45 s on
the iGPU vs ≈62 s on the CPU, and the iGPU costs **zero CPU cores**.
That settles it: **the deep helper is an iGPU model, started on
demand — the CPU stays free.** The CPU remains the fallback handler if
the iGPU is busy (e.g. while FreeToken streams experts through shared
RAM); its 69.6 tok/s prefill is also the faster of the two for
long-context batch work.

Operational notes for the unit:
- **On-demand, not always-on.** A resident second 17.3 GB llama-server
  would share the iGPU's ~26 GB/s with the 3B helper and slow it ~2×.
  Keep the unit stopped; start it on demand (`jarvis-helper --deep`,
  or a systemd socket), stop after idle.
- Model already on disk: the Ollama blob `sha256-58574f2e…` (17.3 GB);
  copy it to `/opt/jarvis-igpu/models/` like the 3B, or point the unit
  at the blob path directly.
- Same shape as `jarvis-engine-igpu.service`: `-dev Vulkan1 -ngl 99`,
  `--ctx-size 8192`, `--jinja`, port 11436.

## Other models this machine can host

| Model | Size (Q4) | Where | Speed | What it adds |
|---|---|---|---|---|
| Qwen3-30B-A3B MoE | 17.3 GB | iGPU, on demand | 11.26 tg / 54.1 pp (measured) | the deep helper (above) |
| llama3.2-1B | 1.2 GB | iGPU | 19.2 tg tok/s (measured) | query router: helper → deep helper → brain escalation |
| bge-m3 or nomic-embed-v2 | 0.5 GB | CPU | ms-level | better embeddings for the memory service (replaces all-MiniLM-L6-v2) |
| bge-reranker-v2-m3 | 2.3 GB | CPU | ms-level | re-ranks memory-service search top-k |
| Whisper large-v3 (faster-whisper int8) | 3 GB | CPU | ~real-time | local STT for the voice channel |
| Qwen3-VL-4B / MiniCPM-V | 3 GB | iGPU or CPU | ≈5–10 tg tok/s (est.) | describe `!photo`/camera images locally |
| Qwen3.8-Flash-Next ~128B MoE | 100 GB (on disk) | 4090 + RAM via FreeToken | pending bench | a **swappable brain**, not a helper |

Notes on the last row: the model (`/home/agent/models/qwen3.8-flash-next/
UD-IQ4_XS`, 3 shards) and the FreeToken 0.2.1 runtime
(`docs/freetoken-runner.md`) are already installed; the CPU-vs-PCIe
bandwidth bench was deliberately not run in that round. It cannot run
while the brain occupies the 4090 (the card OOMs,
`docs/freetoken-runner.md`), so it is a brain *replacement* candidate
for heavy rounds — benchmark first, then decide.

## Suggested tiering

```
 1B (iGPU, optional)      → route: trivial / needs depth / needs the brain
 3B (iGPU, always-on)     → setup questions, fast long-prompt reads
30B-A3B (iGPU, on-demand) → deep questions, harder reasoning
brain 27B (4090)          → the agent's own work
128B MoE (4090, swap-in)  → optional heavy rounds, pending bench
```

The tiering is the real payoff of "two helpers": each level is slower
and smarter, and escalation is explicit. A duplicate 3B would be two
copies of one level.

## Constraints to remember

- **iGPU: keep dense ≤ ~8B, but MoE scales with active params.**
  Dense decode is bandwidth-bound at ~26 GB/s effective (PR #109) and
  falls off a cliff (7B dense: 5.9 tok/s). MoE only touches its
  active weights per token, so 30B-A3B runs at 11.26 tok/s.
- **The iGPU's bandwidth is shared.** Two resident servers split the
  ~26 GB/s — keep the deep helper on demand.
- **4090: no headroom.** Anything bigger than the brain means
  swapping, not adding (that breaks agent runs while it is loaded).
- **CPU: fine for prefill, weak for MoE decode in this build**
  (6.24 ± 4.97 tok/s). Big dense models are out (32B dense ≈ 2 tok/s);
  keep the 16 cores free for voice, memory service, FreeToken.

## Suggested next jobs (each one round)

1. `jarvis-helper --deep` + `jarvis-engine-igpu-deep.service`: copy
   the 17.3 GB GGUF, on-demand unit on port 11436, live latency test
   with real deep questions.
2. FreeToken bandwidth bench for the ~128B MoE (continues job
   chat-1793040169-43b4), then a decision: keep as swap-in brain or
   delete the 100 GB.
3. Memory-service retrieval upgrade: bge-m3 embeddings +
   bge-reranker top-k re-rank, A/B the search quality.
4. 1B router on the iGPU for tier escalation (only if #1 is useful).
