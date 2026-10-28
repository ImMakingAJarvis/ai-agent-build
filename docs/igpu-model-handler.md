# iGPU as a model handler (investigation + adoption)

Job chat-1793010774-0126. Question: can the integrated GPU of the Ryzen
chip serve as another model handler, alongside the 4090 engine?

**Status (2026-10-28, job chat-1793124710-b1cf): adopted.** The iGPU now
runs the always-on **helper model** — Qwen2.5-3B Q4_K_M
(`qwen2.5-3b-instruct-q4_k_m.gguf` from `Qwen/Qwen2.5-3B-Instruct-GGUF`)
serving OpenAI-compatible chat on 11435 via
`jarvis-engine-igpu.service` (enabled, `Restart=always`), queried through
the `jarvis-helper` CLI (setup brief in
`scripts/jarvis-helper-context.md`). Measured on the 3B model:
pp512 ≈ 115 tok/s, tg64 ≈ 13.2 tok/s on Vulkan1 (llama-bench, 2026-10-28).
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

Measured: the iGPU decodes at ~27 GB/s effective — about **45% of the
CPU** (same RAM) and ~1/37 of the 4090. So it is not a speed win; its
only real value is **running an auxiliary model without spending CPU
cores** (the CPU is busy with the agent itself, voice pipeline, memory
service). That niche is small: for interactive chat the 4090 engine's
chat slot is the fast path, and for pure CPU-offload a small model on
the CPU is faster than on the iGPU. Recommendation: keep the staged
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

(Brain reference: qwen3.8 27B Q4_K_M on the 4090 ≈ 48 tok/s decode,
docs/local-brain.md.)

## Reading the numbers

Decode is weight-bandwidth bound: tok/s ≈ effective bandwidth / bytes
per token. From the 7B row: iGPU 5.87 tok/s × 4.68 GB ≈ **27 GB/s
effective**, CPU 10.43 tok/s × 4.68 GB ≈ **49 GB/s** (near the
theoretical 58). The iGPU's Infinity Fabric link to DRAM is simply
narrower than the CPU's, and the iGPU also lost prompt processing
(46 vs 622 tok/s) — its 12 CUs are not much compute. Expect ~10 tok/s
from a 4B-class model on the iGPU, ~20 from the same model on the CPU.

## How to adopt it (done — helper model, 2026-10-28)

Adopted as the always-on helper (see the status note at the top):

1. `scripts/install_igpu_engine.sh` — builds the Vulkan llama.cpp to
   `/opt/jarvis-igpu/bin` (agent-owned), adds group access, installs
   `systemd/jarvis-engine-igpu.service` (disabled by default).
2. The helper GGUF lives at `/opt/jarvis-igpu/models/jarvis-igpu.gguf`
   (3B-class Q4_K_M is the sweet spot on this iGPU), then
   `sudo systemctl enable --now jarvis-engine-igpu`.
3. It serves OpenAI-compatible chat on **port 11435** (port 11434 stays
   the brain). `internal/models.Client` already takes a base URL via
   `OLLAMA_URL`; a second `Client` pointed at 11435 is all the code
   side needs for an auxiliary model. `scripts/install_helper.sh`
   deploys the `jarvis-helper` CLI + context and enables the unit.

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
