---
id: jarvis-jobs
name: Agent job store + jarvis-jobs CLI
summary: The file-based job queue agent-run pulls from — add, next, run, yield, wake, finish, requeue, modes.
group: agent
kind: cli
status: active
binaries: [cmd/jarvis-jobs, cmd/jarvis-migrate, scripts/jarvis-notes]
ports: ["— (files under ~/.local/share/jarvis)"]
docs: [docs/agent-jobs.md, docs/event-driven.md, docs/agent-notes.md]
depends_on:
  - to: redis
    why: "NoBrakes flags (automode, fullauto, keepgoing, running marker)"
  - to: rabbitmq
    why: "capability catalog and mailbox peek for the round prompt"
  - to: memory
    why: "jarvis-notes pushes notes into the vector graph"
---

## Purpose

`internal/agentjob` is the agent's job queue: one directory per job with
`job.json` (spec) and `state.json` (status). `jarvis-jobs` is its CLI and
the round runner: it builds the round prompt and runs the driver. Run
order is class (fix → build → background, re-orderable) then priority
then age; photo-tagged jobs run as code (`jarvis-photo`), not as a model
round. `jarvis-notes` keeps the done/system/asked notes and pushes them
into memory.

## Interfaces

- `jarvis-jobs add|show|list|next|run|finish|requeue|skip|edit|message`
- `jarvis-jobs yield|waiting|wake|mark-running` (park on a result)
- `jarvis-jobs automode|fullauto|keepgoing|nightly|audit|running|order|photo-ready|restart`
- Statuses: ready, claimed, running, waiting, parked, done, failed, skipped.
- `jarvis-migrate` — one-time TASKS.md → queue migration.

## Configuration

- `JARVIS_DATA_DIR` (default `~/.local/share/jarvis`), `JARVIS_CONF`,
  `JARVIS_REDIS`, `RABBITMQ_URL`.
- Round env it sets: `JARVIS_JOB_ID`, `JARVIS_REPLY_QUEUE`, `JARVIS_MODEL`,
  plus the driver's own env (must not be dropped — PR #191).

## Data

- `~/.local/share/jarvis/jobs/<id>/` — job.json, state.json,
  transcript.txt, continuation.md/json, results.json, wake.json.
- `band_order.json`, photo sidecars under `photos/`, inbox under `inbox/`.

## Deploy

    go build -o /tmp/jarvis-jobs ./cmd/jarvis-jobs && sudo install -m 755 /tmp/jarvis-jobs /usr/local/bin/jarvis-jobs

Build from an up-to-date `origin/main` checkout only. agent-run calls it
from PATH every round, so the next round uses the new binary.

## Verify live

    jarvis-jobs list | head
    jarvis-jobs next          # what runs next, in band order
    jarvis-jobs show <id>

## Failure modes

- Installed from a feature branch that predated `requeue -driver`; the
  photo window then stranded the brain (2026-11-07).
- `finish <id> 1` marks a job failed — the argument is the exit code, not
  a count (2026-11-06).
- `finish` on a ready job is a no-op; use `skip` or `mark-running` first.
- A job left `ready` after a deferred round is re-picked immediately;
  `yield` with a deadline parks it (2026-11-16).
