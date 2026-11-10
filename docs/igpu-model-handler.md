# iGPU as a model handler (investigation + adoption)

Job chat-1793010774-0126. Question: can the integrated GPU of the Ryzen
chip serve as another model handler, alongside the 4090 engine?

**Status (2026-10-28, job chat-1793124710-b1cf): adopted.** The iGPU now
runs the always-on **helper model** — Qwen2.5-3B Q4_K_M
(`qwen2.5-3b-instruct-q4_k_m.gguf` from `Qwen/Qwen2.5-3B-Instruct-GGUF`).
Evaluated 2026-11-09 (job chat-1794262130-71d7): NVIDIA's looped
**Dory 1.0 4B** ran at 3.56 tok/s decode on the iGPU — at the hardware
bandwidth ceiling for its 4-loop design but 3.8× slower than the 3B
helper, so the helper **stays Qwen2.5-3B** (see "Dory 1.0 4B" below).
serving OpenAI-compatible chat on 11435, queried through
the `jarvis-helper` CLI (setup brief in
`scripts/jarvis-helper-context.md`). Since 2026-11-08 (job
chat-1794122808-9116) it runs in its own compose container
`jarvis-engine-igpu` (service `engine-igpu`, own port 11435,
`restart: unless-stopped` — photo swap windows stop only the brain
container); the `jarvis-engine-igpu.service` unit is disabled and kept
as the rollback path. Inside the container the iGPU is the only Vulkan
device (no NVIDIA ICD), so the command pins `-dev Vulkan0` (host units
pin `Vulkan1`). Measured on the 3B model:
pp512 ≈ 115 tok/s, tg64 ≈ 13.2 tok/s on Vulkan1 (llama-bench, 2026-10-28).
Full CPU comparison (2026-10-29, job chat-1793248851-31c8): the CPU is
**1.69× faster at decode** (22.9 vs 13.55 tok/s) but the **iGPU is ~2.2×
faster at prefill** (116.7 vs 45–49 tok/s) — see the benchmark table and
"Reading the numbers" below.
The CPU-offload niche from the investigation is exactly what it fills:
the agent consults it for setup-change and self-restart questions without
spending CPU cores.

## TL;DR

**It works, but on this hardware it is slower than the CPU.** The Ryzen
7 7800X3D here has a live RDNA2 iGPU (12 CUs, "Raphael Mendocino",
780M-class) that the kernel already drives (`amdgpu`, Mesa RADV Vulkan).
A Vulkan build of llama.cpp runs models on it, and it runs **in
parallel with the brain on the 4090** with no VRAM contention (the
iGPU reads weights from system RAM).

Measured: the iGPU decodes at ~27 GB/s effective — about **45% (7B) to
59% (3B) of the CPU** (same RAM) and ~1/37 of the 4090 at decode. So it is not a speed win; its
only real value is **running an auxiliary model without spending CPU
cores** (the CPU is busy with the agent itself, voice pipeline, memory
service). That niche is small: for interactive chat the 4090 engine's
chat slot is the fast path, and for decode-heavy CPU-offload a small
model on the CPU is faster than on the iGPU (prefill-bound work is the
one case where the iGPU wins — see below). Recommendation: keep the staged
components (below) as an available option, do not enable the iGPU
engine by default, and do not put anything bigger than ~8B on it.

## Hardware on this machine

| | |
|---|---|
| CPU | AMD Ryzen 7 7800X3D (Raphael, 8C/16T) |
| iGPU | AMD Raphael Mendocino, PCI `1002:164E`, RDNA2, `card0`/`renderD129` |
| dGPU | NVIDIA RTX 4090, `card1`/`renderD128` (brain runs here in Docker, port 11434) |
| RAM | 128 GB DDR5-4800 (running at 3600 MT/s, dual channel ≈ 58 GB/s peak) |
| Display | monitor is on the 4090 (card1-HDMI-A-1); all iGPU outputs disconnected |

The 7800X3D iGPU is often assumed not to exist; on this board it is
enabled and fully driven. Vulkan sees it as `Vulkan1`
(`uma: 1` = shared memory, ~63 GB visible); `Vulkan0` is the 4090.

## What works, what does not

- **llama.cpp (Vulkan backend): works.** Built with
  `-DGGML_VULKAN=ON -DGGML_CUDA=OFF` (see `scripts/install_igpu_engine.sh`).
  The same build sees both GPUs; `-dev Vulkan1` pins the iGPU. Verified
  end-to-end: a `llama-server` on the iGPU (port 11435) served
  `/v1/chat/completions` at 18.8 tok/s decode — matching the iGPU
  benchmark, not the CPU's 34, so the workload really landed on the iGPU.
- **Ollama: no.** The ROCm package targets discrete AMD GPUs; the iGPU
  (gfx10.3) is not on Ollama's/ROCm's supported list. Not worth a full
  ROCm install next to the CUDA stack.
- **ROCm direct: not installed**, same support caveat.
- **Permissions:** the `agent` user needed `video` + `render` groups to
  open `/dev/dri/renderD129` (done: `usermod -aG video,render agent`,
  idempotent in the install script).

## Benchmark

`scripts/igpu-bench.sh` (llama-bench, pp512/tg512, 3 repeats; the 4090
was running the brain, so its numbers are a busy-device lower bound):

| model | device | pp512 tok/s | tg512 tok/s |
|---|---|---|---|
| llama3.2 1B (Q8_0, 1.24B) | iGPU (Vulkan1) | 306.8 ± 0.5 | 19.2 ± 0.02 |
| llama3.2 1B (Q8_0, 1.24B) | CPU (16 threads) | 1669.8 ± 211.8 | 34.0 ± 0.02 |
| llama3.2 1B (Q8_0, 1.24B) | 4090 (Vulkan0) | 33900 ± 16269 | 218.0 ± 1.3 |
| qwen2.5 7B (Q4_K_M, 7.62B) | iGPU (Vulkan1) | 46.1 ± 0.04 | 5.9 ± 0.00 |
| qwen2.5 7B (Q4_K_M, 7.62B) | CPU (16 threads) | 622.0 ± 0.6 | 10.4 ± 0.01 |
| qwen2.5 3B (Q4_K_M, 3.40B) | iGPU (Vulkan1) | 116.7 ± 0.01 ² | 13.55 ± 0.01 ² |
| qwen2.5 3B (Q4_K_M, 3.40B) | CPU (16 threads) | 45–49 ¹ ² | 22.94 ± 0.02 ² |
| qwen2.5 3B (Q4_K_M, 3.40B) | 4090 (Vulkan0) | 17863 ± 62 ² | 283.7 ± 1.1 ² |

¹ **llama-bench's CPU pp512 is wrong in this build.** It reported
3674–3858 tok/s for the 3B CPU prefill, but a live `llama-server` with
`-ngl 0` (same binary, same model, 282-token prompt, two runs) measured
**49.4 and 45.1 tok/s** (`prompt eval time` log line) — ~75× off, and the
reported rate implies ~25 TFLOPS on a ~1.2 TFLOPS chip. The CPU **tg**
rows and all **GPU** rows pass the bandwidth/compute sanity checks and
match live-server measurements, so only the CPU prefill column is
affected; treat the 1B/7B CPU pp values above as suspect in the same
way. For CPU prefill, trust `llama-server`'s `prompt eval time`.
² 2026-10-29, `scripts/igpu-bench.sh` (llama-bench pp512/tg512, 3
repeats) on the helper model `/opt/jarvis-igpu/models/jarvis-igpu.gguf`;
the 4090 row is the busy-device reference (brain was loaded).

(Brain reference: qwen3.8 27B Q4_K_M on the 4090 ≈ 48 tok/s decode,
docs/local-brain.md.)

## Reading the numbers

Decode is weight-bandwidth bound: tok/s ≈ effective bandwidth / bytes
per token. From the 7B row: iGPU 5.87 tok/s × 4.68 GB ≈ **27 GB/s
effective**, CPU 10.43 tok/s × 4.68 GB ≈ **49 GB/s** (near the
theoretical 58). The 3B row agrees: 13.55 × 1.95 ≈ **26 GB/s** (iGPU)
vs 22.94 × 1.95 ≈ **45 GB/s** (CPU). The iGPU's Infinity Fabric link to
DRAM is simply narrower than the CPU's, so the CPU wins decode —
measured **22.9 vs 13.55 tok/s (1.69×)** for the 3B, i.e. the helper
model answers ~1.7× faster if it ran on the CPU.

Prefill, though, flips: it is compute-bound, and the iGPU's FP32 peak
(~6 TFLOPS across 12 RDNA2 CUs) is ~5× the CPU's (~1.2 TFLOPS on 8
Zen4 cores). Measured on the 3B: iGPU **116.7** tok/s (llama-bench;
live-server 105.6) vs CPU **45–49** tok/s — the iGPU pre-fills a long
prompt ~2.2× faster. (The earlier "iGPU lost prompt processing, 46 vs
622" note was based on llama-bench's broken CPU pp numbers, ¹.)

Practical split, confirmed by the 3B: interactive chat (decode-bound,
many tokens per turn) → the CPU would be faster, but the iGPU still
costs zero CPU cores, which is why the helper lives there; long-prompt
batch work (summarizing transcripts) → the iGPU is the better
prefill device *and* still costs no CPU cores.

## Dory 1.0 4B (NVIDIA Nemotron Labs) — evaluated 2026-11-09

Job chat-1794262130-71d7 (Nathan): download NVIDIA's Dory model and
benchmark it as an iGPU and a CPU model, in parallel with the 4090
brain, and switch the iGPU (and later a CPU engine) to it if the
tok/s look decent.

**What it is.** Dory 1.0 4B (2026-10-16, OpenMDW-1.1 license) is a
*looped* (latent-recurrent) normalized GPT: 3.98B params, official
GGUF is Q5_K_M only (3.41 GiB), 256K context. The file's architecture
metadata (read from the GGUF):

- 72 physical blocks = **16 input + 40 recurrent + 16 output**;
  `dory.num_loops` = **4**, so the graph executes
  16 + 40×4 + 16 = **208 layer passes per token** (the 40 recurrent
  weights are re-read once per loop).
- `dory.recurrent_kv_cache_mode` = `per_loop` — each loop gets its own
  KV cache slots, so KV memory scales with the loop count
  (~0.85 MB/token across all 208 cache layers before sliding-window
  reduction; 54/72 blocks are sliding-window, window 1024).
- Attention: 16 q / 4 kv heads (GQA 4:1), head dim 256, embedding
  2560, ffn 7168.
- It is a **thinking model**: the chat template emits
  `reasoning_content` (thinking tokens) before the final answer
  (llama-server enables "preserving reasoning" for it automatically).

**llama.cpp support is not upstream.** Both stock master (3b4d6ab) and
the installed iGPU build (cf24317) fail with
`unknown model architecture: 'dory'`. Working support exists only in
draft PR [ggml-org/llama.cpp#30306](https://github.com/ggml-org/llama.cpp/pull/30306)
(branch `dory-support`, head `1c3cdd2`, 2026-11-02, +671/−28 —
`models/dory.cpp`, per-loop KV mapping in `llama-kv-cache.cpp`).
Experiment-grade, unmerged: treat its numbers as implementation
dependent. Build it with the install script's new overrides:

    LLAMA_REPO=https://github.com/brianchmielNV/llama.cpp \
    LLAMA_REF=dory-support LLAMA_SRC=/opt/jarvis-igpu/src-dory \
    LLAMA_BUILD=/opt/jarvis-igpu/build-dory LLAMA_BIN=/opt/jarvis-igpu/bin-dory \
    install_igpu_engine.sh --build-only

(On this machine that build lives at
`/opt/jarvis-igpu/build-dory/bin/` and the model at
`/opt/jarvis-igpu/models/dory-4b-q5_k_m.gguf`, 3,673,288,864 bytes,
byte-verified against the Hugging Face file.)

**Bench (2026-11-09, llama-bench pp512/tg512; the 4090 brain served
this whole round concurrently — the parallel operation the job asked
for; CPU and iGPU runs strictly sequential):**

| model | device | pp512 tok/s | tg512 tok/s |
|---|---|---|---|
| Dory 1.0 4B (Q5_K_M, 3.98B) | CPU (16 threads) | 61.83 ± 1.47 | 1.16 ± 0.06 |
| Dory 1.0 4B (Q5_K_M, 3.98B) | iGPU (Vulkan1) | 23.31 ± 0.01 | 3.56 ± 0.00 |
| qwen2.5 3B (Q4_K_M, 3.40B) | CPU (16 threads) | 45–49 ¹ | 22.94 ± 0.02 ² |
| qwen2.5 3B (Q4_K_M, 3.40B) | iGPU (Vulkan1) | 116.7 ± 0.01 ² | 13.55 ± 0.01 ² |

The known-broken CPU pp column (¹) did *not* misbehave in the Dory
build (61.8 is a sane value; a live `-ngl 0` server prefill agreed in
magnitude). Live-server decode on the iGPU measured 3.44–3.45 t/s,
matching the bench.

**Reading the numbers.** The loop count is fixed in the file (no
runtime flag in the PR build), so per-token weight traffic is
≈ 3.41 GB × 208/72 ≈ **9.9 GB**. At the iGPU's measured ~27 GB/s
effective bandwidth the ceiling is ~3.7 t/s — the measured 3.56 is
**97% of the hardware ceiling**, i.e. the iGPU path is efficient and
this speed is what the 4-loop design costs on this hardware (the
looping trades compute for accuracy: SWE-bench Verified
50.3/59.5/61.8 at 1/2/4 loops). The CPU sits at 1.16 t/s, only ~25%
of its ~4.5 t/s ceiling — the draft PR forces F32 precision on the
Dory mul_mat nodes (F32 inputs from the recurrent normalizations), so
the CPU path is the under-optimized one; even at its ceiling it would
be 5× slower than the 3B's 22.9. Prefill flips the 3B comparison too:
iGPU 23.3 vs CPU 61.8 (for the 3B the iGPU won 2.2×), i.e. the
per-loop KV handling also costs the Vulkan prefill path. A loop-1 or
loop-2 GGUF, if NVIDIA ever ships one, would scale the iGPU decode
roughly with the loop count (loop 1 ≈ 72 passes ≈ 3.4 GB/token →
~13 t/s class).

**Decision: not adopted.** Pre-declared gate — switch the iGPU to
Dory if tg ≳ 4 t/s and pp512 ≳ 25 with sane output. Measured 3.56 /
23.3: both just under, and 3.56 t/s decode is 3.8× slower than the
3B helper's 13.55, which matters for the voice-pipeline LLM step that
routes to the iGPU for responsiveness. Output quality itself is fine
(thinking + final answer coherent in spot checks). The iGPU helper
**stays Qwen2.5-3B**; no swap was made, so no rollback. The same gate
closes the CPU side: 1.16 t/s vs 22.9 — Dory is not the model for a
CPU lane either. Both conclusions are recorded on Plane (DRY-5, DRY-8,
project DRY); the model and the PR-branch build are kept in place for
a re-test once the PR matures or a lower-loop GGUF appears. The
queue question behind the job — answered in the DRY project notes:
`jobs.cpu`/`jobs.igpu` exist and are consumed by parallel
task-runner workers, but no cpu/igpu model is registered and no CPU
engine exists yet (DRY-6, DRY-7, DRY-9).

## How to adopt it (done — helper model, 2026-10-28)

Adopted as the always-on helper (see the status note at the top):

1. `scripts/install_igpu_engine.sh` — builds the Vulkan llama.cpp to
   `/opt/jarvis-igpu/bin` (agent-owned), adds group access, installs
   `systemd/jarvis-engine-igpu.service` (disabled; rollback path — the
   default runtime is the compose container).
2. The helper GGUF lives at `/opt/jarvis-igpu/models/jarvis-igpu.gguf`
   (3B-class Q4_K_M is the sweet spot on this iGPU), then run the
   container: `docker compose -p ai-agent-build -f docker-compose.yml
   up -d engine-igpu` (or `scripts/install_helper.sh`, which also
   deploys the CLI + context).
3. It serves OpenAI-compatible chat on **port 11435** (port 11434 stays
   the brain). `internal/models.Client` already takes a base URL via
   `OLLAMA_URL`; a second `Client` pointed at 11435 is all the code
   side needs for an auxiliary model. Containers address it as
   `http://jarvis-engine-igpu:11435`; the host uses the published
   127.0.0.1 port.

Sensible uses, in order of worth: (a) none by default; (b) a
background/batch model (summarization of long transcripts) where CPU
cores are the scarce resource; (c) a fallback chat model if the 4090
engine ever dies — 5–10 tok/s is usable for that.

## Risks / caveats

- iGPU models consume system RAM (7B Q4 ≈ 4.4 GB) — negligible on 128 GB.
- A 27B-class model on the iGPU would run at ~1 tok/s: do not do it.
- Splitting the brain across 4090 + iGPU (`-dev Vulkan0,Vulkan1`) is
  theoretically possible but decode is gated by the slowest device's
  bandwidth — it would slow the brain, not speed it. Not recommended.
- The iGPU drives no display here; if a monitor is ever plugged into
  it, display + compute coexist fine on RADV, but re-check.
