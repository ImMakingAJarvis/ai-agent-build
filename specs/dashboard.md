---
id: dashboard
name: Dashboard (queue site)
summary: The JARVIS UI on :8080 — queue, jobs and live logs, machine panel, kanban, plans, secrets, benchmarks, the system map.
group: face
kind: container
status: active
compose: [dashboard]
units: [jarvis-dashboard.service]
binaries: [cmd/dashboard, scripts/install_dashboard.sh]
ports: ["8080 (LAN)"]
health: http://127.0.0.1:8080/
docs: [docs/dashboard.md]
depends_on:
  - to: jarvis-jobs
    why: "reads and edits the agentjob store"
  - to: redis
    why: "live job streams (JARVIS_REDIS)"
  - to: rabbitmq
    why: "queue depths, pending peek, dead letters"
  - to: engine
    why: "machine panel model probe"
  - to: engine-igpu
    why: "machine panel model probe"
  - to: art
    why: "gallery link and photo assist/jobs API"
  - to: nvidia-exporter
    why: "VRAM numbers on the machine panel"
  - to: resourced
    why: "lease and window state on the machine panel"
  - to: kshadow
    why: "Kafka shadow tile: kshadow's state file + management API + Kafka scan"
---

## Purpose

The queue site Nathan uses. Static shell + async sections: queue (bands,
drag to reorder, edit, photo band), agent jobs with live/human-readable
logs, Talk box and Stop, machine panel (loaded models, swap window,
VRAM), the Kafka shadow parity tile, kanban board + comments, plan upload,
dead letters, secrets (write-only), MoE benchmarks, and the **System map**
(`/map`), drawn from `specs/`.

## Interfaces

- Pages: `/`, `/jobs/{id}`, `/logs`, `/kanban`, `/secrets`,
  `/benchmarks`, `/map`.
- HTMX sections: `/html/machine`, `/html/kafka-shadow` (the Kafka shadow
  parity tile — kshadow's state + management counters + a scan of the
  `source=amqp` records; cached 60s server-side), the job/agent-job
  sections.
- APIs: `/api/queue*`, `/api/jobs*`, `/api/owner-message`,
  `/api/kanban/*`, `/api/plans`, `/api/dead-letters*`, `/api/secrets*`,
  `/api/photo/*`, `/api/map`, `/api/map/health`, `/api/map/spec/{id}`.

## Configuration

- Compose env: `ENGINE_URL`, `IGPU_URL`, `ART_URL`, `JARVIS_REDIS`,
  `RABBITMQ_URL`, `KAFKA_BROKERS` (Kafka shadow tile; default
  `kafka:9093`), `NVIDIA_EXPORTER_URL`, `RESOURCED_URL`, `JARVIS_CONF`;
  `env_file: agent.env`; `extra_hosts: host.docker.internal:host-gateway`.
- Kafka shadow tile state path: `~/.local/share/jarvis/kshadow/kshadow-state.json`
  (kshadow's bind-mounted `/data`); `KSHADOW_STATE` overrides.
- Templates and specs are embedded at build time — a template or spec
  change needs an image rebuild.

## Data

- Reads `~/.local/share/jarvis` (job store, kanban, plans, secrets, photo
  sidecars) and `~/.local/state/jarvis` (swap-window state, logs).

## Deploy

    docker compose build dashboard && docker compose up -d dashboard

`docker restart` keeps the old image. **Do not use
`scripts/install_dashboard.sh`** — it installs and starts the legacy host
unit `jarvis-dashboard.service`, which cannot bind :8080 while the
container runs.

## Verify live

    curl -s -o /dev/null -w '%{http_code}\n' http://111.1.2.27:8080/
    curl -s http://111.1.2.27:8080/api/map | head -c 200

Open the changed page from another device on the LAN.

## Failure modes

- Stale binary in the container after a merge (2026-10-26, 11-08).
- Go 1.26 `{{template "x"}}` without `.` renders with nil data — pass `.`
  explicitly (2026-11-06).
- `getPendingQueues` used autoAck and destroyed pending jobs on every
  render (fixed 2026-11-05, PR #169).
- RabbitMQ connection was never started, silently disabling queue views
  (fixed 2026-11-08, EVB-10).
