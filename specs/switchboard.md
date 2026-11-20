---
id: switchboard
name: Switchboard (wait/wake)
summary: Owns the listeners that wake a parked job when its result lands on a flow queue; forward and notify actions too.
group: plumbing
kind: container
status: active
compose: [switchboard]
binaries: [cmd/switchboard]
ports: ["9103 metrics (compose network)"]
health: http://switchboard:9103/metrics
docs: [docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "peeks every listened queue"
  - to: jarvis-jobs
    why: "records results and wakes waiting jobs in the store"
  - to: discord
    why: "notify listeners"
  - to: plane
    why: "yield/wake comments on board-mapped jobs"
---

## Purpose

EVB-14. Durable listener records ("when a message lands on queue Q, do
A") — wake a waiting job, forward to another queue, or notify Discord. A
wake peeks (does not consume) so the woken round reads the result itself.
Times out waits whose deadline passed (reason `timeout`). Resubscribes
after a broker restart.

## Interfaces

- Listener records are created by `jarvis-mq listen` / `ask --wake`.
- Prometheus at `:9103/metrics` (`switchboard_*`).

## Configuration

- `RABBITMQ_URL`, `LISTEN_ADDR`, `JARVIS_CONF`; `env_file: agent.env`.

## Data

- One JSON file per listener under `~/.local/share/jarvis/listeners/`.

## Deploy

    docker compose build switchboard && docker compose up -d switchboard

## Verify live

    docker logs --tail 20 jarvis-switchboard
    jarvis-mq listeners
    bash scripts/e2e-cap-echo.sh          # full submit → yield → wake gate

## Failure modes

- Nil connection after a failed reconnect → SIGSEGV on broker restart
  (fixed 2026-11-09, EVB-17).
