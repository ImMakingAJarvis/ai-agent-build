---
id: image-describe-worker
name: image.describe workers (VLM, gpu + cpu)
summary: Describes images with a small VLM — Qwen3-VL-2B in spare VRAM, an 8B on the CPU when the GPU lease is refused.
group: capability
kind: container
status: active
compose: [image-describe-worker-gpu, image-describe-worker-cpu]
binaries: [cmd/image-describe-worker]
ports: ["— (no listener)"]
docs: [docs/benchmarks/image-describe-vlm-benchmark.md, docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.image.describe"
  - to: resourced
    why: "GPU placement needs a spare-VRAM lease; CPU placement a RAM lease"
---

## Purpose

EVB-21. The brain has no vision, so "look at this picture" is a
capability. Two placements of one binary: `IDESC_MODE=gpu` (Qwen3-VL-2B,
~3.2 GB, runs alongside the brain when VRAM allows) and `IDESC_MODE=cpu`
(the fallback when the GPU lease is refused). Per-image JSON + index in
the Result.

## Interfaces

- Consumes `cap.image.describe`; input refs are image files or a folder.

## Configuration

- `IDESC_MODE`, `IDESC_MIN_FREE_MB` (gpu), `IDESC_THREADS` (cpu),
  `RABBITMQ_URL`, `RESOURCED_URL`, `CAPABILITIES_FILE`.
- Models from `/opt/jarvis-vlm` (ro); llama binaries from
  `/opt/jarvis-engine` (ro).

## Data

- Results under the request's out dir.

## Deploy

    docker compose build image-describe-worker-gpu && docker compose up -d image-describe-worker-gpu image-describe-worker-cpu

## Verify live

    jarvis-mq submit image.describe --input-ref <folder> --reply-to flow.check-$(date +%s) --corr d1

then `jarvis-mq results flow.<that id>`.

## Failure modes

- The GPU worker logs lease refusals while the brain is busy — expected.
- A bare `--reply-to` name (not `flow.*`) drops the Result silently
  (2026-11-10).
- Idle-unload of the VLM coprocess shows as "VLM unreachable" in the
  snapshot — by design.
