---
id: bench-worker
name: bench.run worker
summary: Runs llama-bench or live benchmarks on the iGPU or CPU and replies with the scores.
group: capability
kind: container
status: active
compose: [bench-worker]
binaries: [cmd/bench-worker, scripts/benchmark_q6.py]
ports: ["— (no listener)"]
docs: [docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.bench.run"
  - to: resourced
    why: "RAM lease"
---

## Purpose

EVB-24. A thin adapter over the existing benchmark tooling: llama-bench
mode (a GGUF path or Ollama model name; iGPU via Vulkan or CPU) and live
mode (`benchmark_q6.py` against a base URL). Scores in the Result,
report.md + index.json in the out dir. Never touches the 4090.

## Interfaces

- Consumes `cap.bench.run`.

## Configuration

- `RABBITMQ_URL`, `RESOURCED_URL`, `CAPABILITIES_FILE`; `/dev/dri` for
  Vulkan; models from `/opt/jarvis-igpu` and the Ollama store (ro).

## Data

- Reports under the request's out dir.

## Deploy

    docker compose build bench-worker && docker compose up -d bench-worker

## Verify live

    jarvis-mq submit bench.run --input model=llama3.2:1b --input location=igpu --reply-to flow.check-$(date +%s) --corr b1

## Failure modes

- None recorded since 2026-11-10.
