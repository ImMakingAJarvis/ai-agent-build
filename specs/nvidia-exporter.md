---
id: nvidia-exporter
name: NVIDIA exporter
summary: GPU metrics (nvidia-smi wrapper) for Prometheus and the dashboard's VRAM numbers.
group: monitoring
kind: container
status: active
compose: [nvidia-exporter]
binaries: [scripts/nvidia_prometheus_exporter.py]
ports: ["3001"]
health: http://nvidia-exporter:3001/metrics
docs: [docs/services.md]
depends_on: []
---

## Purpose

Exposes GPU utilization, memory, temperature and power from nvidia-smi.

## Interfaces

- `/metrics` on :3001.

## Configuration

- nvidia runtime; `/usr/bin/nvidia-smi` bind-mounted ro.

## Data

- None.

## Deploy

    docker compose build nvidia-exporter && docker compose up -d nvidia-exporter

## Verify live

    curl -s 127.0.0.1:3001/metrics | head -5

## Failure modes

- None recorded.
