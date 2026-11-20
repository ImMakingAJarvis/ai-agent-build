---
id: art
name: Art service (photo generation + gallery)
summary: Z-Image-Turbo text-to-image and FLUX.2 klein reference images on the 4090, plus the LAN photo gallery with a send form.
group: media
kind: container
status: active
compose: [art]
binaries: [scripts/art_service.py, scripts/install_art.sh, scripts/install_klein.sh, scripts/jarvis-art, scripts/test_art_service.py]
ports: ["8120 (LAN — gallery)"]
health: http://art:8120/health
docs: [docs/art-model.md, docs/brain-swap.md]
depends_on:
  - to: dashboard
    why: "the gallery's Assist and Send buttons call the dashboard photo API"
---

## Purpose

Photo generation. Z-Image-Turbo 6B for text-to-image; FLUX.2 [klein] 4B
lazy-loaded when a reference image is attached. Needs ~12 GB free VRAM, so
it effectively runs only in a swap window (it waits for free VRAM, then
503s with free-vs-needed numbers). Serves the gallery at
`http://111.1.2.27:8120` with timestamps in the viewer's timezone.

## Interfaces

- `POST /generate {prompt, seed?, size?, steps?, model?, refs?}`,
  `GET /health`, `GET /images`, `GET /img/<name>`, `POST /upload`,
  `GET /refs/N`, gallery `GET /`.
- Sizes: aspect tokens (`16x9`, `9x16`) or pixels, long side ≤ 1024.

## Configuration

- `LISTEN_PORT`, `ART_VRAM_WAIT_S`, `ART_MIN_FREE_MB`.
- Code is bind-mounted from `/opt/jarvis-art`.

## Data

- Models `/var/lib/jarvis/art/model`; outputs + sidecar JSON
  `/var/lib/jarvis/art/out`.

## Deploy

    scripts/install_art.sh        # copies art_service.py to /opt/jarvis-art, recreates the container

Klein weights: `scripts/install_klein.sh`.

## Verify live

Only meaningful inside a swap window:

    curl -s 127.0.0.1:8120/health
    curl -s -o /dev/null -w '%{http_code}\n' http://111.1.2.27:8120/

## Failure modes

- Instant 503 OOM while the brain held the VRAM (2026-11-05, PR #164).
- The container only runs during a window, so the gallery is unreachable
  otherwise (2026-11-09).
- Sync FastAPI endpoint returned an un-awaited coroutine on upload
  (2026-11-08).
- Model load needs ~33 GB RAM; with the brain up that risks OOM.
