---
id: worker
name: Queue worker (jarvis-worker)
summary: Consumes the raw RabbitMQ worker queues (voice/fix/image/coding/background) and runs plan_epic planning jobs.
group: plumbing
kind: container
status: active
compose: [worker]
binaries: [cmd/worker, cmd/enqueue, scripts/jarvis-worker.service]
ports: ["9101 metrics (compose network)"]
docs: [docs/agent-jobs.md, docs/work-board/PLAN.md]
depends_on:
  - to: rabbitmq
    why: "consumes the worker queues, emits job.* events"
  - to: engine
    why: "runs text jobs on the brain (ENGINE_URL)"
  - to: engine-igpu
    why: "registry endpoint for the iGPU (IGPU_URL)"
  - to: jarvis-jobs
    why: "ingests image messages as photo jobs into the agentjob store"
  - to: plane
    why: "delivers finished plans (page + sub-items)"
  - to: anthropic-api
    why: "cloud planner/reviewer models from the registry"
---

## Purpose

The raw-queue consumer. Priority order voice > fix > image > coding >
background (the highest non-empty queue is always unpaused). Text jobs
run on the model `pickModel` chooses; `image` messages become photo-tagged
agent jobs; `plan_epic`/`replan_epic` jobs run the Plan → Decompose →
Review pipeline (`prompts/`, `config/models.yaml`) and deliver to Plane.
`enqueue` is the test-publish CLI. Not the same queue as the agentjob
store that agent-run pulls from (docs/system-frame.md §4.2).

## Interfaces

- Consumes `voice`, `fix`, `image`, `coding`, `background`.
- Emits `job.<cap>.started|done|failed` on the `events` exchange.
- Metrics on :9101.

## Configuration

- Compose env: `ENGINE_URL`, `IGPU_URL`, `RABBITMQ_URL`,
  `WORKER_MODELS_FILE=/config/models.yaml`, `WORKER_PROMPTS_DIR`,
  `WORKER_REPO_ROOT`, `DASHBOARD_ADDR`; `env_file: agent.env`.
- Mounts the repo's `config/`, `prompts/` and the repo itself read-only.

## Data

- Job history and metrics; agentjob store entries for ingested jobs;
  plan output (`tasks.json`, `plan.md`) per plan job.

## Deploy

    docker compose build worker && docker compose up -d worker

`scripts/jarvis-worker.service` is the pre-containerization unit (legacy).

## Verify live

    docker logs --tail 20 jarvis-worker
    enqueue --type=fix --id=check-$(date +%s) --prompt="Say ok"   # then watch the log

## Failure modes

- Crash-looped for a day while the broker could not boot (2026-11-04).
- Spoke the Ollama API to a llama.cpp engine (fixed 2026-10-29, PR #113).
- A local planner only runs when the engine's GGUF basename matches the
  registry id (open since 2026-11-02, "D1 naming gap").
