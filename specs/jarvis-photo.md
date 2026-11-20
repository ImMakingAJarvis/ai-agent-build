---
id: jarvis-photo
name: Photo job executor (jarvis-photo)
summary: Runs a photo-tagged job as code — forwards it to image.generate, DMs the image, writes the sent marker, queues any follow-up.
group: media
kind: cli
status: active
binaries: [cmd/jarvis-photo, scripts/install_photo.sh]
ports: ["— (CLI)"]
docs: [docs/brain-swap.md, docs/agent-jobs.md]
depends_on:
  - to: image-gen-worker
    why: "submits cap.image.generate and waits for the Result"
  - to: discord
    why: "delivers the image to Nathan"
  - to: jarvis-jobs
    why: "reads prompt/size/reference sidecars, queues follow-ups"
---

## Purpose

Photo jobs run as code, not as a model round (Nathan, 2026-11-08): the
runner hands a photo-tagged job to `jarvis-photo make --job <id>`. It
reads the sidecars, submits the request, waits for the Result, DMs the
image with its caption and gallery link, writes `photos/<id>.sent`, and
queues the optional follow-up job.

## Interfaces

- `jarvis-photo make --job <id>`.
- A photo round without the `.sent` marker is recorded failed (so a lost
  image is retried, not reported done).

## Configuration

- `ART_GALLERY_URL` (must be the LAN IP — never localhost),
  `PHOTO_ART_WAIT_S`, `RABBITMQ_URL`.

## Data

- Sidecars under `~/.local/share/jarvis/photos/`: `<id>.prompt`,
  `<id>.size`, `<id>.png` (reference), `<id>.followup`, `<id>.sent`.

## Deploy

    scripts/install_photo.sh

## Verify live

`!photo a blue square` → the image arrives in the DM after the next
window, and `ls ~/.local/share/jarvis/photos/<id>.sent` exists.

## Failure modes

- A failed photo round was recorded done and the image silently lost
  (2026-11-06, PR #182).
- The caption's gallery link was loopback until a LAN default landed
  (PR #200).
