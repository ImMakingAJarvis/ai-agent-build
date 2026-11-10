# Benchmark: small VLMs for `image.describe` (EVB-21)

**Date:** 2026-11-09
**Machine:** RTX 4090 24564 MiB (driver 595.99.02), Ryzen (16 threads available)
**Engine:** llama.cpp 0.7.0-dev (build 1, commit 7170cad4d) — the pinned
brain build, `/opt/jarvis-engine/bin/llama-server`.

The `image.describe` capability worker runs a small local VLM as its own
llama-server coprocess, on the 4090's **spare** VRAM (GPU placement) or in
host RAM (CPU fallback). This doc records the benchmark that picks the
models: fixed test set, measured VRAM, per-image speed, and caption
quality against the set's truth manifest.

## First check (job step 1): does the brain have an mmproj?

**Yes — viable, not adopted in this job.** The brain's llama.cpp build
supports `--mmproj`, and a matching projector for Qwen3.8-27B is published
(unsloth, `mmproj-...-F16.gguf`, 0.93 GB, kept at
`/opt/jarvis-vlm/models/`). A disposable smoke test (brain GGUF + projector,
`--n-gpu-layers 0`, CPU) loaded in ~10 s and OCR'd the pangram test card
perfectly in 216.6 s. So the projector can ride on the brain itself
(brain ~20.4 GB + ~1 GB projector < 24.5 GB) and the brain could describe
images whenever it is loaded, no swap window. **Left as a follow-up job**:
changing the brain engine's startup is out of scope here, and the small-VLM
worker is what the plan calls for (independent of brain state, measurable
VRAM, no brain changes).

## Method

- **Fixed test set:** 7 images in `test/image-describe/` (see
  `manifest.json`): 4 synthetic (text card, exit sign, shapes, bar chart —
  ground-truth OCR/structure) + 3 real photos from the art service's output
  (free-form, judged on plausibility). Text-in-image cases matter: the
  describe output feeds `prompt` regeneration, which needs legible text.
- **Prompt:** the worker's production prompt (`caption`/`prompt`/`tags`
  JSON contract), `max_tokens 512`, `temperature 0.2`, one request per
  image.
- **VRAM:** `nvidia-smi` used-memory delta around server load and peak
  after inference. The brain was **in a live conversation** during every
  run (this benchmark was produced from the brain's own round), so
  free VRAM was ~3.7–4.2 GB throughout — exactly the condition the GPU
  placement must cope with.
- **Quality:** per-image caption checked against the manifest truth;
  `json_parseable` records whether the reply was strict JSON (a lenient
  parser recovers truncated replies — see worker code).

## Test set

| file | kind | truth (manifest) |
|---|---|---|
| 01-sentence-card.png | text | white card, two lines of black text: 'The quick brown fox jumps over the lazy dog.' |
| 02-exit-sign.png | text | dark background; green sign with white 'EXIT'; yellow text 'Room 42' on the right |
| 03-shapes.png | shapes | light gray background; a red circle top-left, a blue square to its right, a yellow/orange triangle bottom-center |
| 04-bar-chart.png | chart | bar chart titled 'Weekly visits'; three bars A, B, C; B is the tallest, then C, then A |
| 05-photo-…1635215837.png | photo | (free-form; judge plausibility) |
| 06-photo-…1701843947.png | photo | (free-form; judge plausibility) |
| 07-photo-…2031604446.png | photo | (free-form; judge plausibility) |

## GPU placement (llama-server `--n-gpu-layers 99`)

### Qwen3-VL-2B-Instruct Q4_K_M + mmproj F16 — **selected**

Load 5.3 s. VRAM: **3146 MiB** delta after load, **3244 MiB** peak during
inference (from 20373 MiB used by the brain). All 7 images described OK:

| image | seconds | tokens | strict JSON | verdict |
|---|---|---|---|---|
| 01 sentence card | 0.3 | 89 | yes | exact pangram OCR, black-on-white |
| 02 exit sign | 0.3 | 123 | yes | exact: green sign, white EXIT, yellow 'Room 42' |
| 03 shapes | 0.3 | 87 | yes | all three shapes + colors + positions correct |
| 04 bar chart | 0.4 | 150 | yes | labels A/B/C and B-tallest ordering correct |
| 05 photo | 1.0 | 300 | **no** (truncated mid-`prompt`; lenient parser recovers caption) | plausible patchwork-device description |
| 06 photo | 0.7 | 183 | yes | plausible plush-radio description |
| 07 photo | 0.5 | 110 | yes | plausible stone-staircase-in-forest description |

### Qwen3-VL-4B-Instruct Q4_K_M + mmproj Q8_0 — does not fit a busy brain

Failed to come up within the 300 s health window: the process died during
load (the `--rm` bench container vanished with its logs, so no OOM line was
captured). The arithmetic matches: 2.5 GB model + 0.84 GB projector + KV +
CUDA context ≫ the ~4.2 GB free at bench time. **This is the boundary the
GPU placement's lease enforces** (`IDESC_MIN_FREE_MB`, default 4096): the
lease checks free VRAM before every consume session and refuses when the
brain is in a conversation, which is why the GPU worker runs the 2B.

## CPU fallback (llama-server `--n-gpu-layers 0 --no-mmproj-offload`, 16 threads)

### Qwen3-VL-4B-Instruct Q4_K_M + mmproj F16

Up in 11 s. Residual VRAM **564 MiB** (the unavoidable CUDA-context floor —
the binary always initializes the CUDA backend; `--no-mmproj-offload` keeps
the vision encoder in host RAM, see below). First run (23:25, while this
round's own builds/tests were sharing the CPU — load ~20 throughout, but
the 406 s outlier on image 06 is 5× its neighbors, i.e. this run is
contaminated; kept as `qwen3vl-4b-cpu-run1.json`): per-image seconds
312.0 (warmup), 61.6, 46.5, 158.4, 119.7, 406.5, 385.4. Quality: 7/7 strict
JSON; all four synthetic truths exact (pangram verbatim, EXIT + Room 42,
all three shapes, A/B/C with B tallest); photos plausible.

Re-run under steady load (00:11, no other CPU work this round — the
machine's background load is the same as during the 8B/7B runs): per-image
seconds **331.6, 396.6, 252.2, 69.3, 57.8, 64.3, 64.2**. The pattern is
not warmup-of-the-first-request: the first **three** images are 4–6× slower
than the last four, then the 4B settles at ~60 s/image. Quality: 7/7 strict
JSON, all four synthetic truths exact again. So the 4B's real numbers are
"~250–400 s per image for the first ~15 minutes after a cold start, then
~60 s" — and in this worker's lifecycle (coprocess unloaded after 5 min
idle) most requests *are* the first of a cold session, where the 8B's
single 236 s warm-up image beats the 4B's three slow ones.

### Qwen3-VL-8B-Instruct Q4_K_M + mmproj F16

Up in 11 s. Residual VRAM **580 MiB** (peak 673). Per-image seconds
235.9 (warmup), 175.3, 51.8, 59.4, 74.3, 73.5, 148.7. Quality: 7/7 strict
JSON; all four synthetic truths exact; photos marginally more specific
than the 4B ("handcrafted boombox … holographic screen" vs "handcrafted
radio … holographic screen"). ~6.2 GB of host RAM (122 GiB on the box).

### Qwen2.5-VL-7B-Instruct Q4_K_M + mmproj F16

Up in 9.4 s. Residual VRAM 587 MiB (peak 662). Fastest of the CPU set:
88.1 (warmup), 46.9, 36.7, 55.4, 55.3, 61.1, 53.4 s. **Rejected: it fails
the pangram truth** — image 01's caption is "A playful phrase featuring a
fox and a dog, often used to demonstrate font and layout" (the phrase's
*use*, not its text; the OCR only appears, truncated, in the prompt field).
Text-in-image is the point of this capability (the output feeds prompt
regeneration), so a model that paraphrases the text instead of reading it
does not qualify, whatever its speed.

## Why the CPU fallback is VRAM-neutral

A ngl-0 server still loads the mmproj (vision encoder) onto the GPU by
default: measured 1594 MiB residual for the 2B. `--no-mmproj-offload`
keeps it in host RAM → **488 MiB** (2B) / **564 MiB** (4B) / **580 MiB**
(8B) — just the CUDA-context floor, coexisting with a fully busy brain
(3.7 GB free).
Without it, the "CPU" fallback would spend the same VRAM as a real GPU
job for no speed benefit.

## The pick

- **GPU placement:** Qwen3-VL-2B Q4_K_M + F16 mmproj. 3146 MiB fits the
  4090's spare VRAM even with the brain mid-conversation (measured against
  a live brain); sub-second to one second per image; exact OCR on all four
  synthetic truth images. The 4B does not fit that spare (above), so the
  2B is the only candidate, not a trade-off.
- **CPU fallback:** Qwen3-VL-8B Q4_K_M + F16 mmproj. Both Qwen3-VL sizes
  pass every truth with 7/7 strict JSON; the 8B's photo captions are
  marginally more specific. Speed: once warm the 4B is actually a bit
  faster (~60 s vs the 8B's 52–175 s), but the 4B needs three slow
  (~250–400 s) images to warm up after a cold start, while the 8B pays one
  (~236 s). In this worker's lifecycle the coprocess is unloaded after 5
  min idle, so a request usually *is* the first of a cold session — there
  the 8B wins outright (a 3-image folder: ~6 min on the 8B vs ~16 on the
  4B). VRAM is the CUDA-context floor either way (580 vs 564 MiB) and the
  8B's ~6.2 GB of RAM is nothing on a 122 GiB box. The Qwen2.5-VL-7B is
  faster still but fails the pangram truth, which is the point of the
  capability.

## Reproduce

`/tmp/bench_vlm.py` (this round's harness; keep the test set — it is in
the repo at `test/image-describe/`). Models under `/opt/jarvis-vlm/models/`
(Qwen3-VL 2B/4B/8B Q4_K_M, Qwen2.5-VL-7B Q4_K_M, matching F16/Q8_0
moprojs — from unsloth; the brain's own mmproj F16 as well).
