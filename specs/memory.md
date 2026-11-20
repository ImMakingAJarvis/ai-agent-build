---
id: memory
name: Memory service (vector graph)
summary: Local vector search over job specs, transcripts, the queue, repo docs, notes and kanban comments.
group: memory
kind: container
status: active
compose: [memory]
units: [jarvis-memory.service]
binaries: [scripts/memory_service.py, scripts/install_memory.sh, scripts/download_memory_model.sh]
ports: ["8101 (127.0.0.1; compose name memory)"]
health: http://memory:8101/health
docs: [docs/agent-notes.md, docs/agent-claude.md]
depends_on: []
---

## Purpose

SQLite + all-MiniLM-L6-v2 (ONNX) embeddings over everything the agent
produces. The agent searches it before grepping ("what did we do about
X"); chat and voice use it for context; `jarvis-notes add` pushes notes in
immediately.

## Interfaces

- `POST /search {query, k, source?}`, `POST /index`, `GET /stats`,
  `GET /health`.
- CLI: `python3 scripts/memory_service.py --search "..." | --stats | --reindex`.

## Configuration

- `MEMORY_HOST`, `MEMORY_PORT`. Code from `/opt/jarvis-memory` (ro).
- `systemd/jarvis-memory.service` is the legacy host unit (disabled).

## Data

- `~/.local/share/jarvis/memory/memory.db` + the ONNX model.

## Deploy

Copy `scripts/memory_service.py` to `/opt/jarvis-memory`, update
`DEPLOYED_FROM`, then

    docker compose restart memory memory-reindex

**Do not run `scripts/install_memory.sh`** — it enables the legacy host
units, which would fight the containers for :8101.

## Verify live

    curl -s 127.0.0.1:8101/health
    curl -s -X POST 127.0.0.1:8101/search -H 'Content-Type: application/json' -d '{"query":"wake word latency","k":3}' | head -c 300

## Failure modes

- Ran from a script that existed only on a branch; a restart would have
  crash-looped it (2026-10-26).
- Reindex prune compared the DB with itself, so stale docs were never
  dropped (fixed 2026-11-12, PR #247).
