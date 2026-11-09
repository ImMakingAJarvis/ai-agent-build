# Art model: photo generation (Z-Image-Turbo)

> **Added 2026-10-28** (job `chat-1793014077-2dd0`). The stack can now
> generate photos locally. Service: `jarvis-art` (compose `art`), port
> 8120 on 127.0.0.1. CLI: `jarvis-art generate "..."`.

## What runs

- **Model:** Z-Image-Turbo — 6B text-to-image DiT (Tongyi-MAI), 8-step
  turbo distillation (guidance-free), trained at 1024×1024. Weights: the
  official `Tongyi-MAI/Z-Image-Turbo` pipeline repo (32.9 GB F32 on disk,
  ~13 GB resident when loaded in bf16).
- **Serving:** `scripts/art_service.py` — a small FastAPI app in the
  `jarvis/art:local` container (python 3.12 + torch cu128 + diffusers),
  same pinned-deploy-point pattern as voice (`/opt/jarvis-art`).
- **VRAM:** the brain owns ~20 GB of the 4090's 24.5 GB, and the model is
  bigger than the remainder in every component (transformer ~11.5 GB bf16,
  text encoder ~3.75 GB), so the pipeline runs with
  `enable_sequential_cpu_offload()` — each *submodule* streams to the GPU
  just before its forward pass and back to RAM after. Near-zero resident
  VRAM; tens of seconds per image. One generation at a time; a second
  `POST /generate` gets 409.

## API

```bash
curl -s 127.0.0.1:8120/health                                  # status/GPU/last
curl -s 127.0.0.1:8120/images?limit=10                          # newest PNGs
curl -s -X POST 127.0.0.1:8120/generate \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"a photo of a red panda in snow","seed":42}'
# -> {"path":"/var/lib/jarvis/art/out/20261028-...-42.png", ..., "seconds":N}
```

`POST /generate` fields: `prompt` (required), `seed` (int), `size`
(`"WxH"` pixels, each dim capped at 1024 — or an aspect ratio, written
with small numbers like `"16x9"` / `"16:9"` / `"9x16"`: the long side is
scaled to 1024 and both sides rounded to a multiple of 16, the VAE patch,
so `16x9` → 1024×576, `4x3` → 1024×768, `21x9` → 1024×432), `steps`
(default 9 = 8 DiT forwards). A bad `size` is a 400.
Generations take tens of seconds — allow a few minutes for the HTTP
request.

CLI:

```bash
jarvis-art generate "a photo of the house at dusk" --seed 7
jarvis-art list
```

Images land in `/var/lib/jarvis/art/out/` as `YYYYMMDD-HHMMSS-<seed>.png`
(the next round's gallery site will serve that directory).

## Why this and not the alternatives

- **Ollama image generation: dead end on this box.** The Ollama library's
  image models (`x/flux2-klein`, `x/z-image-turbo`) are **MLX-only** —
  `ollama pull` on Linux (0.40.7) fails with "this model requires MLX
  support, but the MLX runtime is not available". Community ports
  (e.g. `jmorgan/z-image-turbo`) need a pre-release Ollama. Verified
  empirically, not from docs.
- **ComfyUI** would work but is a heavyweight web app; the stack's pattern
  is small single-purpose services with a plain HTTP API, and the only
  consumer is machine-to-machine (the Discord daemon, next job).
- **Z-Image-Turbo** over Flux.1-schnell (12B): half the weights, 8 fast
  steps, modern quality. (The `T5B/Z-Image-Turbo-FP8` port looked like a
  6 GB shortcut but is single-file transformer weights — no
  `model_index.json`, not a loadable diffusers pipeline; verified.) If
  quality ever needs a bump, the service only changes `ART_MODEL_REPO` +
  re-running `scripts/install_art.sh`.

## Thinking-mind prompts (added 2026-10-29, job chat-1793252974-720f)

Photo requests no longer go straight from the owner's words to
Z-Image-Turbo. The thinking mind (`jarvis-brain` on the engine) first
turns a casual request into a detailed image prompt — subject kept
exact, plus setting, light, composition, camera style, palette and
mood; no in-scene text (the model renders it badly). Already-detailed
requests pass through almost unchanged. Code: `internal/photomind`
(wired into `cmd/jarvis-chat` for both the primary and Hermes bots).

Discord commands:

- `!photo <text>` — the mind expands the prompt, the image is
  generated, and the caption shows the (truncated) prompt that was
  actually used.
- **Sizes:** any of the commands may start with a size token —
  `!photo 16x9 <text>` — to request a non-square frame: `WxH` pixels
  (each dim ≤ 1024) or an aspect ratio (`16x9`, `9x16`, `4x3`, `21x9`,
  `16:9`, ...; the long side becomes 1024 and both sides land on the
  VAE's 16-pixel grid). The token rides to the generation as a
  `*.size` sidecar next to the prompt, and the thinking mind is told
  the frame it is writing for, so it composes for the shape. Without a
  token the default square 1024×1024 applies.
- `!photoassist <text>` — same, but the mind's full prompt is also
  sent as a chat message first, so it can be read, reused, or refined
  by giving the mind more context.
- `!photocheck <text>` — iterates with the generator: draft prompt →
  generate → the mind critiques the prompt against the original
  request (it cannot see the image, so it judges the prompt) →
  improved prompt → generate again. The final image is sent with a
  note. If the critique finds nothing to change, the second pass is
  skipped; if it dies after the first pass the first image is still
  sent.

If the brain is unreachable, generation falls back to the owner's raw
words and the caption says so. CLI equivalents:
`jarvis-art enhance "<request>"` prints the mind's prompt;
`jarvis-art generate "<request>" --mind` expands then generates.

## Reference photos: FLUX.2 [klein] (added 2026-11-08, job chat-1794200798-5afb)

The second model is **FLUX.2 [klein] 4B** (BFL) — a reference-capable
pipeline: given one or more reference images it keeps their subject (a
real person) while generating the prompt's scene around it. Weights:
`black-forest-labs/FLUX.2-klein-4B` (~23 GB) in
`/var/lib/jarvis/art/models/flux2-klein-4b`, via
`scripts/install_klein.sh`. It is **lazy-loaded on the first reference
generation** (a plain photo never pays for the 23 GB load), and because
photo work runs in the brain-swap window (the 4090 is free) the
transformer uses plain model CPU offload, not sequential.

New API surface:

- `POST /upload` — raw image bytes (png/jpeg/webp, ≤ 25 MB) staged under
  `/var/lib/jarvis/art/refs` as `upload-<ts>-<hex>.<ext>`; returns
  `{"name": ...}`.
- `GET /refs/<name>` — a staged reference (basename only, no traversal).
- `POST /generate` — new fields `model` (`"zimage"` default | `"klein"`)
  and `refs` (staged names). **Any `refs` routes the generation to
  klein** — an uploaded photo is meaningless to plain text-to-image.
  Klein's defaults: 4 steps, guidance 1.0. `/health` reports
  `klein_weights` / `klein_loaded` / `klein_error`.

How a photo gets a reference:

- **Discord:** `!photo` / `!photoassist` / `!photocheck` with an image
  attached — the first png/jpeg/webp attachment is downloaded and stored
  as the job's reference sidecar.
- **Gallery page** (the `GET /` on :8120): a form with prompt, size, a
  file input, **Assist** (POST `:8080/api/photo/assist` — the thinking
  mind expands the basic request, like `!photoassist`) and **Send**
  (uploads the file to `/upload`, then POSTs `:8080/api/photo/jobs`,
  which queues the same photo-tagged job Discord queues). The dashboard
  endpoints are CORS-enabled because the page is served from :8120.

Both flows write the reference to the job's sidecar
(`<data>/photos/<id>.png`). At generation time, `jarvis-photo make --job
<id>` stages a copy as `ref-<jobid>-<unix>.png` in the refs directory
(bound into the art container) and calls `/generate` with
`model: "klein"` + the staged name; a job without a sidecar generates on
Z-Image-Turbo as before.

## Operations

- **Install/refresh:** `scripts/install_art.sh` (deploy code to
  `/opt/jarvis-art`, download weights to `/var/lib/jarvis/art/model`),
  then `docker compose up -d art`. First start loads the pipeline in
  ~1–2 min.
- **Klein weights:** `scripts/install_klein.sh` (skips when
  `model_index.json` is present; `ART_KLEIN_REPO` / `ART_KLEIN_DIR`
  override the HF repo and target). The refs directory
  (`/var/lib/jarvis/art/refs`) is created by `install_art.sh`.
- **Swap model:** `ART_MODEL_REPO=<repo> scripts/install_art.sh` (any
  diffusers text-to-image pipeline works; `guidance_scale=0` and the step
  count assume a distilled turbo — revisit for a non-turbo model).
- **No systemd unit** — the container is managed by `jarvis-stack`
  (`docker compose up -d`) like every other service.
- **Gotcha:** the container runs as uid 1001 (agent); keep
  `/var/lib/jarvis/art` agent-owned so generated PNGs stay readable by
  host-side tools.
