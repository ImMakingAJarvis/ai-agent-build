---
id: prometheus
name: Prometheus
summary: Scrapes the engines, workers, exporters, switchboard, resourced and token ledger; 180-day retention.
group: monitoring
kind: container
status: active
compose: [prometheus]
ports: ["9090"]
health: http://prometheus:9090/-/healthy
docs: [docs/services.md]
depends_on:
  - to: engine
    why: "llama_engine job (llamacpp:* metrics)"
  - to: node-exporter
    why: "host metrics"
  - to: nvidia-exporter
    why: "GPU metrics"
  - to: token-ledger
    why: "all-time token counters"
  - to: switchboard
    why: "switchboard_* metrics"
  - to: resourced
    why: "lease and window metrics"
  - to: kafka
    why: "kafka_* exporter metrics (jarvis-kafka-exporter:9308)"
---

## Purpose

The metrics store behind Grafana. Retention 180 d (long enough for
per-month buckets; the all-time token total lives in the token ledger).

## Interfaces

- UI/API on :9090; `/-/healthy`.

## Configuration

- `docker/monitoring/prometheus/prometheus.yml` (mounted). Scrape targets
  use **container names**, not compose aliases.

## Data

- Named volume `prometheus_data`.

## Deploy

    docker compose up -d prometheus      # config change: docker compose restart prometheus

## Verify live

    curl -s 127.0.0.1:9090/-/healthy
    curl -s '127.0.0.1:9090/api/v1/targets' | grep -o '"health":"[a-z]*"' | sort | uniq -c

## Failure modes

- Targets on a compose alias the recreated worker lost (2026-10-29,
  PR #113).
- Quoted metric names with colons parse as strings in PromQL — write
  them unquoted.
- A bind-mounted config rewritten in place (new inode) while the
  container runs — the container keeps serving the old unlinked inode
  (visible as `…//deleted` in `/proc/self/mountinfo`) and reload picks
  up nothing. `docker compose up -d --force-recreate` the service
  (found 2026-11-22: the kafka job was absent for 11 days this way).
