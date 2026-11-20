---
id: engine-voice
name: Voice call engine
summary: A second copy of the brain with a short context, started only for a voice call window.
group: brain
kind: container
status: optional
compose: [engine-voice]
ports: ["11436 (127.0.0.1)"]
health: http://engine-voice:11436/health
docs: [docs/voice-call.md]
depends_on: []
---

## Purpose

The conversational model for a voice call (Nathan, 2026-11-18): the same
27B GGUF as the brain with a fresh short context (`--parallel 1
--ctx-size 32768`) so a call gets fast, short turns. It runs inside a
voice swap window: the brain steps out and this engine takes the GPU.

## Interfaces

- OpenAI-style API + `GET /health` on :11436.
- Started by `jarvis-swap out --voice`; resourced exposes
  `POST/DELETE /window/voice`.

## Configuration

- Compose profile `voice` — it never starts with `docker compose up -d`.
- Same image and mounts as `engine` (`jarvis/engine:local`,
  `/opt/jarvis-engine`).

## Data

- None of its own (reads the brain's model).

## Deploy

Image is shared with `engine`: `docker compose build engine`. It is
started and stopped by the voice window, not by hand.

## Verify live

During a call window:

    curl -s 127.0.0.1:11436/health

Outside a window it is expected to be absent (the map shows it down).

## Failure modes

- New on 2026-11-18; nothing recorded yet.
- Port 11436 is also the default for `ft serve` (FreeToken MoE benches) —
  do not run both.
