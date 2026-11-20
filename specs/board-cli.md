---
id: board-cli
name: Board CLIs (jarvis-board, plane-epic, jarvis-kanban, migrations)
summary: CLIs the agent uses on the boards — Plane item comment/state/done, new epic projects, local kanban show/comment.
group: board
kind: cli
status: active
binaries: [cmd/jarvis-board, cmd/plane-epic, cmd/jarvis-kanban, cmd/board-migrate, cmd/plane-migrate-projects, scripts/plane-setup.sh, scripts/install_kanban_cli.sh]
ports: ["— (CLI)"]
docs: [docs/work-board/NOTES.md, docs/plane-to-queue.md]
depends_on:
  - to: plane
    why: "items, comments, states, projects"
  - to: dashboard
    why: "jarvis-kanban reads and writes the kanban store the dashboard serves"
---

## Purpose

- `jarvis-board item|link|comment|state|done <REF>` — model-prefixed
  Plane comments and state moves (`$JARVIS_MODEL`).
- `plane-epic create "<title>" --identifier XXX` — the only supported way
  to add a Plane project (one project per epic); updates `PLANE_PROJECTS`
  and restarts plane-bridge.
- `jarvis-kanban show|comment|comments <id>` — the local kanban.
- `board-migrate`, `plane-migrate-projects` — one-time migrations
  (2026-10-31); `scripts/plane-setup.sh` — idempotent board setup.

## Interfaces

- Plane API v1 (`X-Api-Key`) and session API (pages, webhooks).
- Kanban file store `~/.local/share/jarvis/kanban/`.

## Configuration

- `PLANE_URL`, `PLANE_API_KEY`, `PLANE_WORKSPACE`, `PLANE_PROJECT`,
  `PLANE_PROJECTS` from `agent.env`.

## Data

- Kanban: `~/.local/share/jarvis/kanban/{tasks,epics,comments}/`.

## Deploy

    go build -o /tmp/jarvis-board ./cmd/jarvis-board && sudo install -m 755 /tmp/jarvis-board /usr/local/bin/jarvis-board
    scripts/install_kanban_cli.sh

## Verify live

    jarvis-board item <REF>
    jarvis-kanban show <id>

## Failure modes

- Flags after positional args are ignored by Go flag parsing — flags go
  first (polluted AAB-61, 2026-10-31; `plane-epic` usage line, 2026-11-09).
- Board card numbers do not equal plan task numbers (offset +1, later +2)
  — target cards by title.
