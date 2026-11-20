---
id: wake-queue
name: Wake-queue bridge
summary: Publishes wake-word events as high-priority voice jobs on RabbitMQ (optional, not running today).
group: plumbing
kind: container
status: optional
compose: [wake-queue]
units: [wake-queue.service]
binaries: [cmd/wake-queue]
ports: ["— (no listener)"]
docs: [docs/services.md]
depends_on:
  - to: rabbitmq
    why: "publishes voice jobs"
---

## Purpose

Bridges wake-word detection events (source chosen by `WAKE_SOURCE`) into
the worker's `voice` queue at high priority. Wake word and STT now happen
on the Home Assistant satellite, so this is not part of the running stack.

## Interfaces

- Publishes to the `voice` queue.

## Configuration

- `RABBITMQ_URL`, `JARVIS_CONF`, `WAKE_SOURCE`; `env_file: agent.env`.
- Compose profile `optional`; `systemd/wake-queue.service` is the legacy
  host unit.

## Data

- None.

## Deploy

    docker compose --profile optional up -d wake-queue

## Verify live

    docker logs --tail 20 jarvis-wake-queue

## Failure modes

- None recorded; it has not run since containerization.
