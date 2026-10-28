# Art model: photo generation (Z-Image-Turbo)

> **Added 2026-10-28** (job `chat-1793014077-2dd0`). The stack can now
> generate photos locally. Service: `jarvis-art` (compose `art`), port
> 8120 on 127.0.0.1. CLI: `jarvis-art generate "..."`.

## What runs

- **Model:** Z-Image-Turbo — 6B text-to-image DiT (Tongyi-MAI), 8-step
  turbo distillation (guidance-free), 1024×1024. Weights: the official
  `Tongyi-MAI/Z-Image-Turbo` pipeline repo (32.9 GB F32 on disk, ~13 GB
  resident when loaded in bf16).
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
(`"WxH"`, each dim capped at 1024), `steps` (default 9 = 8 DiT forwards).
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

## Operations

- **Install/refresh:** `scripts/install_art.sh` (deploy code to
  `/opt/jarvis-art`, download weights to `/var/lib/jarvis/art/model`),
  then `docker compose up -d art`. First start loads the pipeline in
  ~1–2 min.
- **Swap model:** `ART_MODEL_REPO=<repo> scripts/install_art.sh` (any
  diffusers text-to-image pipeline works; `guidance_scale=0` and the step
  count assume a distilled turbo — revisit for a non-turbo model).
- **No systemd unit** — the container is managed by `jarvis-stack`
  (`docker compose up -d`) like every other service.
- **Gotcha:** the container runs as uid 1001 (agent); keep
  `/var/lib/jarvis/art` agent-owned so generated PNGs stay readable by
  host-side tools.
