---
id: llm-small-worker
name: llm.small workers (7B batch, gpu + cpu)
summary: Batch sub-tasks (summarise, classify, extract) on Qwen2.5-7B — spare VRAM when available, CPU otherwise.
group: capability
kind: container
status: active
compose: [llm-small-worker-gpu, llm-small-worker-cpu]
binaries: [cmd/llm-small-worker]
ports: ["— (no listener)"]
docs: [docs/benchmarks/llm-small-model-benchmark.md, docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.llm.small"
  - to: resourced
    why: "GPU placement needs a 6.5 GB spare-VRAM lease"
---

## Purpose

EVB-23. Qwen2.5-7B-Instruct Q4_K_M for batch work the brain should not
spend a round on: `task` = summarise | classify | extract | custom over
input text files. Batch only — conversation fallback is the iGPU helper.

## Interfaces

- Consumes `cap.llm.small`; Result `Data.result` (text or parsed JSON)
  plus `result.json`.

## Configuration

- `LMS_MODE`, `LMS_MIN_FREE_MB` (gpu), `LMS_THREADS` (cpu),
  `RABBITMQ_URL`, `RESOURCED_URL`, `CAPABILITIES_FILE`.
- Model from `/opt/jarvis-llm-small` (ro).

## Data

- Results under the request's out dir.

## Deploy

    docker compose build llm-small-worker-gpu && docker compose up -d llm-small-worker-gpu llm-small-worker-cpu

## Verify live

    jarvis-mq submit llm.small --input task=summarise --input-ref <file.txt> --reply-to flow.check-$(date +%s) --corr s1

## Failure modes

- CPU mode is slow (~2 tok/s); a summary took ~5 minutes (2026-11-10).
