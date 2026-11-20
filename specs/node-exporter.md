---
id: node-exporter
name: Node exporter
summary: Host CPU, RAM and disk metrics for Prometheus.
group: monitoring
kind: container
status: active
compose: [node-exporter]
ports: ["9100"]
health: http://node-exporter:9100/metrics
docs: [docs/services.md]
depends_on: []
---

## Purpose

Stock `prom/node-exporter` with the host root bind-mounted read-only
(`--path.rootfs=/host`).

## Interfaces

- `/metrics` on :9100.

## Configuration

- Compose `command:` only. No systemd collector (no systemd in the
  container).

## Data

- None.

## Deploy

    docker compose up -d node-exporter

## Verify live

    curl -s 127.0.0.1:9100/metrics | grep -c '^node_filesystem_avail_bytes'

## Failure modes

- None recorded.
