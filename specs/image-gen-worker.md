---
id: image-gen-worker
name: image.generate worker
summary: Capability adapter over the art service — generates a photo inside a swap window and replies with the PNG path.
group: capability
kind: container
status: active
compose: [image-gen-worker]
binaries: [cmd/image-gen-worker]
ports: ["— (no listener)"]
docs: [docs/event-driven.md, docs/brain-swap.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.image.generate"
  - to: art
    why: "waits for the model, then POST /generate"
  - to: resourced
    why: "12 GB GPU lease — needs_brain_out, so it runs in a window"
---

## Purpose

EVB-20. Consumes `cap.image.generate`, waits (bounded) for the art model
to load, generates (a reference image routes to the klein model), copies
the PNG into the request's out dir and replies with the artifact path,
seed, size and time. `jarvis-photo` forwards `!photo` jobs to it.

## Interfaces

- Consumes `cap.image.generate`; reply on the request's flow queue.

## Configuration

- `ART_URL=http://art:8120`, `RABBITMQ_URL`, `RESOURCED_URL`,
  `CAPABILITIES_FILE`.

## Data

- PNGs in the request's out dir; the art service keeps its own copy.

## Deploy

    docker compose build image-gen-worker && docker compose up -d image-gen-worker

## Verify live

Queue a photo (`!photo a blue square` or `jarvis-jobs add ... -tags photo
-prompt "a blue square"`), let the window open, then check the DM and
`docker logs --tail 20 jarvis-image-gen-worker`.

## Failure modes

- A message without a prompt burns its retry budget and dead-letters (by
  design).
- Leftover messages on `cap.image.generate` keep a window open with
  nothing useful to do (2026-11-12) — purge stale requests.
