---
id: engine
name: Brain engine (jarvis-brain)
summary: The local brain — llama.cpp serving Qwen3.8-27B on the 4090; every local round, chat and voice reply runs on it.
group: brain
kind: container
status: active
compose: [engine]
binaries: [scripts/install_engine.sh]
ports: ["11434 (127.0.0.1; compose name engine)"]
health: http://engine:11434/health
docs: [docs/local-brain.md, docs/worker-moe-incident-2026-11-04.md, docs/brain-swap.md]
depends_on: []
---

## Purpose

The brain: `llama-server` (pinned llama.cpp b11815) serving
`jarvis-brain.gguf` — Qwen3.8-27B Q4_K_M — with a 128K context
(`--ctx-size 131072`), 2 parallel slots, unified q4_0 KV cache and
Prometheus metrics. It is the model behind `agent-run` local rounds
(Claude Code talks to it through `ANTHROPIC_BASE_URL`), the Discord bot,
voice and the worker. This is the **only** brain deployment: there is no
systemd brain (removed 2026-11-06, PR #184; Nathan chose compose as
canonical 2026-11-08).

## Interfaces

- OpenAI-style `/v1/models`, `/v1/chat/completions`, `/v1/completions`;
  Anthropic-style messages API used by Claude Code; `GET /health`
  (200 ready, 503 while loading).
- `GET /metrics` — `llamacpp:*` counters/gauges (scraped by prometheus,
  folded by token-ledger).
- Slot save/restore to `/var/lib/jarvis/slots`.

## Configuration

- All flags are the compose `command:` (model path, ctx, parallel,
  cache types, chat template `/opt/jarvis-engine/templates/qwen3.8-chat-template.jinja`).
- CUDA 13.1 libs bind-mounted from `/usr/local/cuda-13.1/lib64`;
  `LD_LIBRARY_PATH` set in compose. Image base is ubuntu:26.04 because the
  binary links glibc 2.43.
- Claude Code side: `scripts/agent-local` sets
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS=131072` to match.

## Data

- Model, binary, templates: `/opt/jarvis-engine` (read-only mount).
- Slots: `/var/lib/jarvis/slots`.
- VRAM: ~18.5 GB of the 24.5 GB at idle; less free while serving.

## Deploy

    docker compose build engine && docker compose up -d engine

Only between rounds — the engine serves the round you are in. Model or
binary swaps: `scripts/install_engine.sh`, `scripts/pull_models.sh`.

## Verify live

    curl -s 127.0.0.1:11434/health
    curl -s 127.0.0.1:11434/v1/models | head -c 200      # lists jarvis-brain

`jarvis-stack-status` reports model, quant, ctx and VRAM.

## Failure modes

- **A local round stopped it** (`docker compose stop engine`): every later
  round died on ECONNREFUSED, 4 h 57 m dark (2026-11-04). `restart:
  unless-stopped` does not restart an explicit stop. Now blocked by
  `scripts/brain-guard-hook.sh`; the watchdog re-arms it.
- Two deployments fighting for :11434 (systemd unit vs container,
  2026-10-29 → 11-08) — resolved: container only. Never start a host brain.
- Zombie container "Up" on no network after a boot race (2026-11-05).
- Stopped on purpose during a swap window (photo, voice call, game) — the
  window controller brings it back; a stranded window leaves it down
  (2026-11-06, 11-07).
- No image input: the brain has no mmproj loaded, so a round that tries to
  Read an image fails ("image input is not supported").
