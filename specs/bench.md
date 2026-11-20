---
id: bench
name: T2I benchmark service (jarvis-bench)
summary: Picks the next art model — candidate models × fixed prompt set, blind swipe voting, per-model metrics (in progress).
group: media
kind: container
status: active
compose: [bench]
binaries: [scripts/install_bench.sh]
ports: ["8121 (127.0.0.1)"]
health: http://bench:8121/health
docs: [docs/art-model-options.md]
depends_on: []
---

## Purpose

The text-to-image benchmark (T2I plan, Plane project T2I). Skeleton +
20-prompt set so far: FastAPI, SQLite, YAML model/prompt config. It runs
in the swap window alongside art.

## Interfaces

- `GET /health` (includes GPU free VRAM), `GET /api/status`.

## Configuration

- `BENCH_HOST`, `BENCH_PORT`. Kill switch: `touch
  /var/lib/jarvis/bench.disabled` (idles, API 503).
- Code from `/opt/jarvis-bench` (reuses the `jarvis/art:local` image).

## Data

- DB `~/.local/share/jarvis/bench/bench.db`; images and weights
  `/var/lib/jarvis/bench/{imgs,models}`.

## Deploy

    scripts/install_bench.sh

## Verify live

    curl -s 127.0.0.1:8121/health

## Failure modes

- The installer did not deploy `bench/imgs`, so input assets never
  reached the runtime (fixed 2026-11-17, PR #257).
