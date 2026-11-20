---
id: grafana
name: Grafana
summary: The Home AI dashboards (GPU, CPU, disk, engine throughput, historical tokens, machine view).
group: monitoring
kind: container
status: active
compose: [grafana]
ports: ["3000"]
health: http://grafana:3000/api/health
docs: [docs/services.md]
depends_on:
  - to: prometheus
    why: "the only datasource"
---

## Purpose

Dashboards provisioned from the repo; reloaded within ~30 s of a file
change, no restart needed.

## Interfaces

- UI/API on :3000.

## Configuration

- Datasource provisioning `docker/monitoring/grafana/provisioning/`;
  dashboards `grafana/` (both mounted ro). The Prometheus datasource URL
  must be `http://prometheus:9090` — `localhost` resolves inside the
  Grafana container.

## Data

- Named volume `grafana_data`.

## Deploy

    docker compose up -d grafana

## Verify live

    curl -s 127.0.0.1:3000/api/health

Then open the changed panel and confirm it has data.

## Failure modes

- Every panel empty: datasource pointed at `localhost:9090`
  (2026-10-30, PR #118).
- Panels read worker counters real engine traffic never touches
  (2026-11-03, PR #138).
- Panel type is `barchart`, not `bargraph`; `1M` is not a PromQL duration.
