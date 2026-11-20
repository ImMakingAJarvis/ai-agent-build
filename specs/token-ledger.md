---
id: token-ledger
name: Token ledger
summary: Folds the engines' resetting token counters into an all-time total for Grafana's historical token panels.
group: monitoring
kind: container
status: active
compose: [token-ledger]
binaries: [cmd/token-ledger]
ports: ["9102 (compose network)"]
health: http://token-ledger:9102/metrics
docs: [docs/services.md]
depends_on:
  - to: engine
    why: "polls /metrics"
  - to: engine-igpu
    why: "polls /metrics"
---

## Purpose

`llamacpp:*` counters reset on every engine restart. The ledger polls
both engines, folds restarts into a persisted cumulative and re-exposes
it as `jarvis_tokens_total{engine,kind}` (2026-11-07, PR #187).

## Interfaces

- `/metrics` on :9102.

## Configuration

- `ENGINES` (name=url list), `LISTEN_ADDR`, `POLL_INTERVAL`, `STATE_DIR`.

## Data

- `~/.local/share/jarvis/token-ledger/state.json` — the all-time total;
  deleting it resets history.

## Deploy

    docker compose build token-ledger && docker compose up -d token-ledger

## Verify live

    docker exec jarvis-prometheus wget -qO- http://token-ledger:9102/metrics | grep jarvis_tokens_total | head

## Failure modes

- llama.cpp prints large counters in scientific notation ("1.72e+06"),
  which broke parsing (fixed 2026-11-07).
