# Art model options — text-to-image models we could run here

> Added 2026-10-29 (job `chat-1793307553-f424`, extended by Nathan:
> "research all text to image models I could run, even one that can't sit
> fully in vram. All options, with blurbs, and an estimate for how long
> they'd take"). 2025–2026 entries verified on Hugging Face 2026-10-29;
> older entries from prior knowledge — re-verify before installing.

## What the machine allows

- **RTX 4090, 24.5 GB.** The brain (27.3B Q4, ~16.5 GB) sits in VRAM
  while loaded, leaving **~2.7 GB free** (`/health` on :8120, 2026-10-29).
  The art service therefore runs everything with
  `enable_sequential_cpu_offload()` — weights stream RAM→GPU per forward.
- **122 GB RAM** (~60 GB free): any model whose weights fit in ~55 GB
  runs, just slower.
- **Disk: 54 GB free** on `/` — the current Z-Image-Turbo weights already
  take 31 GB. Realistically **one large model at a time**; prefer fp8/GGUF
  for the big ones.
- **Measured anchor:** Z-Image-Turbo (6B, 8 steps, 1024², offloaded) =
  **~30 s/image** on this box. All estimates below scale from that anchor.

## Current config (verified live 2026-10-29)

**Model: Z-Image-Turbo** (Tongyi-MAI, 6B T2I DiT, bf16, ~31 GB on disk),
**text-to-image only** — no img2img, no reference/subject images, no
inpaint (see `docs/art-model.md` for API and ops). ~30 s/image. The Tongyi-MAI
org on HF lists only `Z-Image-Turbo` and `Z-Image` (base) — **there is no
Z-Image-Edit**; reference images would need a different model.

## TL;DR table

Estimates: 1024², sequential CPU offload (brain loaded), one-at-a-time.
"ref" = takes reference/subject images; "i2i" = can transform a given
image (pipeline mode, not a model feature — any DiT can).

| Model | Params | ref? | Steps | Est. s/image | Disk (bf16) | License |
|---|---|---|---|---|---|---|
| PixArt-Σ | 0.6B | i2i | 20–30 | ~10–20 s | ~1.5 GB | Apache |
| SDXL Turbo/Lightning | 2.6B | i2i | 1–4 | ~10–20 s | ~5 GB | open |
| SDXL | 2.6B | i2i | 25–30 | ~60–90 s | ~7 GB | open |
| MOSS-Image (Base/SFT) | 5B | bbox edits only | 50 | ~3–4 min | ~10 GB | Apache |
| **Z-Image-Turbo (current)** | 6B | — | 8 | **~30 s (measured)** | 31 GB | Apache |
| Z-Image (base, hi-quality) | 6B | — | 50 | ~3 min | ~31 GB | Apache |
| FLUX.2 [Klein] 4B | 4B | **input images** | ~4–8 | ~15–40 s | ~8 GB | BFL dev (NC) |
| Qwen-Image-2.1-Turbo | 7B | **multi-ref** | 8 | ~30–60 s | ~14–40 GB | Qwen research (NC) |
| Qwen-Image-2.1 | 7B img-side | **multi-ref, mask** | 40 | ~3–4 min | ~14–40 GB | Qwen research (NC) |
| Fibo-2.0-dev | 8B | **up to 4 ref, mask** | 28–50 | ~2.5–5 min | ~16 GB | CC BY-NC 4.0 |
| Chroma (1-HD / -Flash) | 9B | — (FT base) | 40 | ~4–6 min | ~18 GB | Apache |
| FLUX.1-schnell | 12B | i2i | 4 | ~1–2 min | ~24 GB (fp8 ~12) | NC |
| FLUX.2 [flash]¹ | 12B | optional photos¹ | 8 | ~1–2 min | ~24 GB (fp8 ~12) | NC |
| Krea-2-Turbo / -Raw | 13B | i2i (community) | 8 (turbo) | ~1.5–2.5 min | ~26 GB | Krea community (gated) |
| FLUX.1-dev | 12B | i2i | 50 | ~8–10 min | ~24 GB | NC |
| FLUX.1-Kontext | 12B | **reference editing** | 50 | ~8–10 min | ~24 GB | NC |
| HiDream-I1 | 17B | i2i | 30–50 | ~5–10 min | ~34 GB | open |
| Qwen-Image 1.0 / Edit-2509 | 20B | **ref (Edit)** | 30–50 | ~6–12 min | ~40 GB | Qwen (NC) |
| HunyuanImage 2.1 | 20B | i2i | 30–50 | ~6–12 min | ~40 GB | TCNC |
| FLUX.2 [dev] | **32B** | **reference editing** | 28–50 | ~15–25 min (fp8/Q4) | 64 GB bf16 / ~32 fp8 | BFL dev (NC) |
| flux-3-image-klein | 6B | i2i (new line) | — | unknown — early | ~12 GB | BFL |

¹ FLUX.2 [flash] from BFL's Nov 2025 release; not re-verified on the org
page 2026-10-29 — check before installing. "NC" = non-commercial license,
fine for this personal project.

## Current-gen options (verified on HF 2026-10-29)

### Qwen-Image-2.1 / 2.1-Turbo (Qwen)
7B image-side single-stream DiT ("7B params" per card; full pipeline with
text encoder is larger — verify download size). Text-to-image **plus image
editing, multi-reference composition** ("keep identity for people and
products"), masked local edits, RGBA output, improved in-image text
rendering. Turbo is an 8-step distillation with built-in sampling schedule
and KV caching of text/reference context; loads via
`QwenImage21Pipeline` in diffusers. The plain model uses 40 steps.
GGUF (unsloth) and quantized ports (PrunaAI) exist if RAM/disk get tight.
**Best reference-image candidate for this box: same speed class as the
current model (8 steps) with multi-reference.**

### FLUX.2 family (Black Forest Labs)
- **FLUX.2 [dev] — 32B** rectified-flow transformer (verified: "32B
  params", not 12B). Generates and edits from text **and optional
  photos** (reference editing), 28 steps "a good trade-off" (50 default),
  FLUX non-commercial license. bf16 = 64 GB won't fit the 54 GB free
  disk — use fp8 (~32 GB) or GGUF.
- **FLUX.2 [Klein] 4B** — "generate or edit images from text prompts with
  optional input images"; small, fast, edit-capable.
- **FLUX.2 [Klein] 9B** (+ 9B-KV fp8 variants) — "generate or edit images
  from text and optional photos".
- **FLUX.2 [flash]¹** — 8-step distilled dev (verify).
- **flux-3-image-klein (6B)** — the new FLUX.3 line: i2i foundation
  checkpoint (32 single-stream DiT layers) + a 9B "pup" prompt-expansion
  model + a ComfyUI port. Early; no speed data.
- **FLUX.1 [dev]/[schnell]/[Kontext] (12B)** — still current-ish: dev is
  the quality t2i workhorse (50 steps), schnell is the 4-step turbo,
  **Kontext does reference-image editing** and is the previous-gen
  answer to the reference-image question.

### Fibo-2.0-dev (Bria)
8B flow-matching model operating over FLUX.2 VAE latents. t2i (short
prompt → structured VGL caption → image), prompt-driven editing,
mask-confined regeneration, and **up to 4 reference inputs** merged into
one output (try-on, style transfer) plus an "Inspire" mode. 50 steps
(28 a trade-off), guidance 5.0; an "Edit Turbo" variant is the low-step
option. CC BY-NC 4.0. The card itself points at RTX 4090/5090 + 4-bit as
the local recipe.

### Krea-2-Turbo / -Raw (Krea.ai)
12–13B DiT, open-sourced: **Krea-2-Raw** (base) and **Krea-2-Turbo**
(fine-tuned + distilled, **8 steps**, guidance 0). t2i; the card doesn't
document reference inputs (community spaces hint at identity editing).
Gated "Krea 2 Community License" — download requires agreeing to the AUP.

### MOSS-Image Base / SFT (OpenMOSS)
5B, Apache 2.0. t2i from natural language, tag sequences, or **JSON
layouts** (controlled text placement / object arrangement) plus bbox-based
controlled editing. 50 steps, flow-matching Euler, CFG 3. No
reference-image feature. Small, permissive, and fast for its quality —
a good layout-control option.

### Chroma 1-HD / Flash (lodestones)
9B (8.9B, BF16) built from FLUX.1-schnell with architectural swaps
(3.3B timestep-encoding layer → 250M FFN). Deliberately a **neutral
foundation for finetuning**, not a style. 40 steps, CFG 3.0, Apache 2.0.
Their fp8/int8 (gemlite) route cut a 40-step run from ~66 s to ~27.7 s on
their GPU — the fp8 route is how we'd run it here.

### Z-Image (base) (Tongyi-MAI)
The non-turbo 6B sibling of the current model: same architecture,
"high quality" at ~50 steps vs Turbo's 8. t2i only. ~3 min/image
estimated (50 × the per-step cost of the measured Turbo).

## Previous-gen (known; still downloadable, re-verify)

- **FLUX.1** dev/schnell/Kontext (12B) — see above; Kontext is the
  pre-FLUX.2 reference-editing model.
- **Stable Diffusion 3.5** Medium (2.5B) / Large (8B) / **Large Turbo**
  (8B, ~8 steps) — solid t2i, i2i supported; Large Turbo ≈ 1–2 min here.
- **SDXL** (2.6B) + **SDXL Turbo / Lightning / Hyper** (1–4 steps) — the
  speed kings (~10–20 s), full img2img support; older quality, weaker
  prompt adherence than the DiTs above.
- **HiDream-I1** (17B) — strong text rendering in images; i2i; ~5–10 min.
- **HunyuanImage 2.1** (20B) — good quality, TCNC license, ~6–12 min.
- **Qwen-Image 1.0 / Qwen-Image-Edit-2509** (20B) — the pre-2.1 line;
  Edit-2509 did single-image editing + reference; superseded by 2.1.
- **Lumina-Image 2.0** (7B) — rectified-flow, clean t2i, ~3–5 min.
- **AuraFlow** (12B) — rectified-flow transformer, decent, ~6–10 min.
- **PixArt-Σ** (0.6B) — the smallest practical model; ~10–20 s; also the
  only one that could plausibly run on the **iGPU**
  (stable-diffusion.cpp Vulkan) — expect minutes, not seconds.
- **Sana** (1.6B–12B) — efficient DiT family, fast at the small end.
- **Janus-Pro** (13B) — unified understanding+generation; subject-driven
  (reference) generation; diffusers support is community-grade.
- **SD 1.5 / 2.1** (0.9B/1.4B) — legacy; only if something must run on
  almost nothing.

## Checked but won't fit

- **HunyuanImage 3.0** (255B MoE, 13B active): ~510 GB bf16, ~250 GB fp8,
  ~130 GB Q4 — all beyond 122 GB RAM / 54 GB disk. Not runnable here.

## Reference-image capability — the original question

- **Current (Z-Image-Turbo): no.** t2i only; no Z-Image-Edit exists on HF.
- **True subject/reference generation:** Qwen-Image-2.1(-Turbo),
  FLUX.2 [dev]/[flash]/[Klein], Fibo-2.0 (up to 4 refs), FLUX.1-Kontext,
  Janus-Pro.
- **img2img (transform a given photo):** pipeline-level — *any* DiT above
  can do it once the service exposes it; it is not what "reference"
  means (no subject consistency).
- **Controlled/bbox editing:** MOSS-Image, Fibo-2.0, Qwen-Image-2.1
  (masks).

## How the time estimates work

Anchor: Z-Image-Turbo 6B × 8 steps = ~30 s measured → ~3.75 s/step
offloaded at 1024² (compute-bound on the 4090; 6B bf16 ≈ 12 GB streamed
per pass fits PCIe). Scaled by params × steps: 5B×50 ≈ 3–4 min,
12B×50 ≈ 8–10 min, 32B×28 (quantized) ≈ 15–25 min. Turbo/distilled
models (1–8 steps) are the fast lane regardless of size. ±50% — these are
planning numbers, not benchmarks.

**Free-VRAM mode:** if the brain is idle-unloaded from the GPU (the
engine supports idle-unload), models ≤ ~16 GB (fp8) run fully in VRAM —
roughly 5–10× faster (schnell ~10–15 s, Z-Image-Turbo ~8 s,
Qwen-2.1-Turbo ~15–20 s). Unverified as a combined brain+art workflow;
the `!photo` flow calls the brain first, so timing depends on the
engine's unload latency.

## What switching costs

- **Swap for another t2i model:** `ART_MODEL_REPO=<repo>
  scripts/install_art.sh` — but `scripts/art_service.py` assumes a
  distilled turbo (guidance 0, ~8 steps, `ZImagePipeline`). A non-turbo
  model needs its pipeline class + steps/guidance/VAE settings in the
  service (one small code change per model).
- **Reference images:** service must accept an input image (file in
  `/var/lib/jarvis/art` or an upload) and pass it as the pipeline's
  reference input; the Discord daemon's `!photo` must accept an attached
  photo. Medium change, model choice above decides the rest.
- **Disk:** delete the current 31 GB before pulling another big model;
  keep quantized weights for >20 GB pipelines.

## Recommendation

1. **Reference images wanted → Qwen-Image-2.1-Turbo.** Multi-reference +
   editing + masks at 8 steps, diffusers-native, ~30–60 s here — same
   speed class as today with the capability we lack. NC license is fine
   for personal use.
2. **Fast + reference → FLUX.2 [Klein] 4B** (~15–40 s), if BFL's NC
   license is acceptable and the 4B quality holds up.
3. **Top quality, slow → FLUX.2 [dev] 32B (fp8)**: reference editing at
   ~15–25 min/image; only if a photo is worth a quarter-hour.
4. **Multi-reference try-on/styling → Fibo-2.0** (up to 4 refs, CC BY-NC).
5. **Keep Z-Image-Turbo** if t2i-only at ~30 s stays good enough; add
   img2img to the service later — that is an orthogonal, model-free
   change.
