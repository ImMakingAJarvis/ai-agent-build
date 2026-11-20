---
id: plane-bridge
name: Plane bridge
summary: Turns Plane board changes (webhooks + catch-up sync) into agent jobs and plan/run_task messages; syncs plan/model/run labels.
group: board
kind: host-service
status: active
units: [plane-bridge.service]
binaries: [cmd/plane-bridge]
ports: ["8102 (webhook receiver, /healthz)"]
health: http://host.docker.internal:8102/healthz
docs: [docs/plane-to-queue.md, docs/work-board/NOTES.md, docs/work-board/PLAN.md]
depends_on:
  - to: plane
    why: "webhooks in, API calls out (bot session for webhook management)"
  - to: rabbitmq
    why: "publishes plan_epic and run_task jobs (jobs.gpu/cpu/igpu)"
  - to: jarvis-jobs
    why: "ingests board jobs into the agentjob store"
---

## Purpose

The board → queue path. Receives HMAC-verified Plane webhooks for every
project in `PLANE_PROJECTS`, runs a catch-up sync on start, and turns
board changes into work: a `plan:<model>` label queues planning, Plan
approved moves sub-items to Ready, a Ready card queues a `run_task`.
Keeps `plan:`/`model:`/`run:` labels in step with `config/models.yaml`
and posts fit-check comments.

## Interfaces

- `POST /webhook` (Plane), `GET /healthz` on :8102.
- Publishes to the plan queue and the location queues
  `jobs.gpu`/`jobs.cpu`/`jobs.igpu`.

## Configuration

- `EnvironmentFile: ~/.config/jarvis/agent.env` — `PLANE_URL`,
  `PLANE_API_KEY`, `PLANE_BOT_EMAIL`/`PLANE_BOT_PASSWORD`,
  `PLANE_PROJECTS`, webhook secret file; `BRIDGE_LISTEN` (default
  `:8102`); `BRIDGE_MODELS_FILE`.
- `plane-epic create` adds a project to `PLANE_PROJECTS` and restarts it.

## Data

- Event log `~/.local/share/jarvis/plane-bridge/events.jsonl`.

## Deploy

    go build -o /tmp/plane-bridge ./cmd/plane-bridge && sudo install -m 755 /tmp/plane-bridge /usr/local/bin/plane-bridge && sudo systemctl restart plane-bridge

## Verify live

    curl -s 127.0.0.1:8102/healthz
    journalctl -u plane-bridge -n 20 --no-pager     # "plan label sync done"

## Failure modes

- A stale binary rejected the whole `models.yaml` ("unknown engine
  whisper") and froze label sync for hours (2026-11-10).
- Silent 400 on every webhook: payload shape assumed wrong (2026-10-31).
- Kept dead publishers after a broker restart (fixed 2026-11-06, PR #173).
- Plane rate limits (429) on bulk calls — the client backs off.
- Read only the first page of a project's labels and states: a
  `plan:`/`model:` label past page 1 was dropped from cards ("no model:
  label on the card") and the label sync re-created it every 15 s (HTTP
  409), stopping at that project (2026-11-19 → 11-20). Lists now follow
  every cursor page; the sync handles each project on its own and counts a
  409 as present.
- BWT-1 sat in Planning for ~50 min: ~13 catch-ups evaluated it and
  queued nothing, with no log line (2026-11-20). The quiet paths were a
  card with no model-bot assignee, a bot that is not the *first* assignee
  (only `Assignees[0]` was read), and an earlier plan job for the card
  already in the store; which one hit BWT-1 was not confirmed. Every
  assignee is now checked; a card that still is not
  planned gets one `bridge: not planning — …` comment (no bot assignee,
  or its plan job failed) and a `planning card not queued` log line.
