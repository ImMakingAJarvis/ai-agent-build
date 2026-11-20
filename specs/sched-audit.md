---
id: sched-audit
name: Weekly notes audit (sched-audit)
summary: On each weekly tick, queues one background agent job that audits the notes and instruction files.
group: plumbing
kind: container
status: active
compose: [sched-audit]
binaries: [cmd/sched-audit]
ports: ["— (no listener)"]
docs: [docs/scheduled-work.md]
depends_on:
  - to: rabbitmq
    why: "consumes sched.weekly"
  - to: jarvis-jobs
    why: "enqueues audit-weekly-<week> into the agentjob store"
---

## Purpose

Subscriber on `sched.weekly`. Each tick enqueues one idempotent job
`audit-weekly-<week>`: a brain round that reads the standing notes, both
CLAUDE.md files, the memory dir and the docs index, and writes
`notes/audits/audit-<date>.md` (Still needed / Could use changing /
Should be removed). No model call in the subscriber itself.

## Interfaces

- Consumes `sched.weekly` (prefetch 1, manual ack).

## Configuration

- `RABBITMQ_URL`, `TZ`, `SCHED_WEEK_DAY`.

## Data

- Writes into the agentjob store (`~/.local/share/jarvis`).

## Deploy

    docker compose build sched-audit && docker compose up -d sched-audit

## Verify live

    jarvis-jobs show audit-weekly-<YYYY-MM-DD>

## Failure modes

- The audit only proposes; nothing applies it unless a job is queued
  (2026-11-16 audit applied 2026-11-18).
