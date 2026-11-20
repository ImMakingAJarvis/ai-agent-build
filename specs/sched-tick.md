---
id: sched-tick
name: Cadence ticker (sched-tick)
summary: The cron — fires one sched.<cadence>.tick event at each hourly/daily/weekly/monthly boundary.
group: plumbing
kind: container
status: active
compose: [sched-tick]
binaries: [cmd/sched-tick]
ports: ["— (no listener)"]
docs: [docs/scheduled-work.md]
depends_on:
  - to: rabbitmq
    why: "publishes sched.*.tick on the events exchange"
---

## Purpose

Polls the wall clock (30 s) and fires one tick per crossed boundary into
the durable `sched.{hourly,daily,weekly,monthly}` queues. No catch-up and
no replay on startup. Any service may subscribe to a cadence queue.

## Interfaces

- Publishes `sched.<cadence>.tick` (routing key) to `events`.
- `sched-tick fire <cadence>` — manual tick.

## Configuration

- `RABBITMQ_URL`, `TZ=America/Los_Angeles`, `SCHED_WEEK_DAY=1`.

## Data

- None.

## Deploy

    docker compose build sched-tick && docker compose up -d sched-tick

## Verify live

    docker exec jarvis-sched-tick app fire hourly    # then check sched.hourly depth

## Failure modes

- None recorded since 2026-11-10.
