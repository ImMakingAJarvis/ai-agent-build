---
id: engine-igpu
name: iGPU helper engine (jarvis-igpu)
summary: Always-on small model (Qwen2.5-3B) on the Ryzen iGPU — chat/voice fallback, igpu rounds, jarvis-helper.
group: brain
kind: container
status: active
compose: [engine-igpu]
units: [jarvis-engine-igpu.service]
binaries: [scripts/install_igpu_engine.sh, scripts/install_helper.sh, scripts/jarvis-helper]
ports: ["11435 (127.0.0.1; containers use jarvis-engine-igpu:11435)"]
health: http://jarvis-engine-igpu:11435/health
docs: [docs/igpu-model-handler.md, docs/local-brain.md]
depends_on: []
---

## Purpose

Vulkan llama.cpp on the 7800X3D iGPU serving Qwen2.5-3B Q4_K_M with a
32K context. It never touches the 4090, so it stays up through swap
windows. Uses: chat and voice fallback while a job is running, the `igpu`
round driver (jobs that need the 4090 free), and the `jarvis-helper` CLI
(setup questions from `scripts/jarvis-helper-context.md`).

## Interfaces

- OpenAI-style API + `GET /health` + `GET /metrics` on :11435.
- `jarvis-helper "question"` (host CLI, `-s` session, `--raw`, `--status`).

## Configuration

- Compose `command:` (`-dev Vulkan0 -ngl 99 --ctx-size 32768 --metrics`).
- `/dev/dri` passed through; numeric GIDs (44/990) because the daemon
  cannot resolve group names.
- The `igpu` driver's context (`IGPU_CONTEXT_TOKENS`) must match `--ctx-size`.

## Data

- Binary + model: `/opt/jarvis-igpu` (read-only); the model file is
  `/opt/jarvis-igpu/models/jarvis-igpu.gguf`.
- Helper brief: `/opt/jarvis-helper/context.md` (installed from the repo).

## Deploy

    docker compose build engine-igpu && docker compose up -d engine-igpu
    scripts/install_helper.sh          # helper CLI + brief

The host unit `jarvis-engine-igpu.service` is **disabled legacy**
(rollback only) — enabling it while the container runs collides on 11435.

## Verify live

    curl -s 127.0.0.1:11435/health
    docker exec jarvis-dashboard wget -qO- http://jarvis-engine-igpu:11435/health
    jarvis-helper --status

## Failure modes

- Containers pointing at `host.docker.internal:11435` cannot reach it (the
  host port binds 127.0.0.1) — fixed by recreating them with the compose
  URLs (2026-11-10, PR #224).
- The old watchdog resurrected the disabled unit and stole the port during
  cutover (2026-11-08).
- `-driver igpu` rounds silently ran on the brain because the runner
  dropped the driver env (2026-11-07, PR #191).
- 3B prose is weak; it is fine for tool use and mechanical steps only.
