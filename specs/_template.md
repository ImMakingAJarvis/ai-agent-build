---
id: my-service
name: My service
summary: One line — what it is for.
group: plumbing
kind: container
status: active
compose: [my-service]
units: []
binaries: [cmd/my-service]
ports: ["9999 (127.0.0.1)"]
health: http://my-service:9999/health
docs: []
depends_on:
  - to: rabbitmq
    why: "consumes the my.queue queue"
---

## Purpose

What it does and why it exists.

## Interfaces

- `GET /health` on :9999 — 200 when ready.
- Consumes `my.queue`; publishes `events` `my.*`.

## Configuration

- `RABBITMQ_URL` — compose environment.

## Data

- Nothing persistent.

## Deploy

    docker compose build my-service && docker compose up -d my-service

Confirm: `docker inspect -f '{{.Created}}' jarvis-my-service` is after the merge.

## Verify live

    docker exec jarvis-my-service wget -qO- http://127.0.0.1:9999/health

## Failure modes

- None recorded yet.
