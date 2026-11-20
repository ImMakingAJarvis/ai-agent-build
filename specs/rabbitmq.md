---
id: rabbitmq
name: RabbitMQ broker
summary: AMQP broker for every queue (worker, location, cap.*, flow.*, mailboxes, cadence, events, dead letters) plus MQTT for Home Assistant.
group: plumbing
kind: container
status: active
compose: [rabbitmq]
ports: ["5672 AMQP, 15672 mgmt, 15692 metrics (127.0.0.1)", "1883 MQTT (LAN)"]
health: tcp://rabbitmq:5672
docs: [docs/event-driven.md, docs/scheduled-work.md]
depends_on: []
---

## Purpose

The message broker. Worker queues `voice`/`fix`/`image`/`coding`/
`background` (priority via `x-max-priority`), location queues
`jobs.gpu|cpu|igpu`, capability queues `cap.<cap>`, reply queues
`flow.<id>`, mailboxes `agent.<name>.inbox`, cadence queues
`sched.<cadence>`, the `tasks` and `events` topic exchanges, and the
`jarvis.dlx` → `dead` dead-letter path. The MQTT plugin on 1883 carries
the Home Assistant sensors.

## Interfaces

- AMQP 5672, management UI/API 15672, Prometheus 15692, MQTT 1883.
- Every queue is listed with owner/purpose in docs/event-driven.md,
  "Queues".

## Configuration

- `rabbitmq/` mounted at `/etc/rabbitmq` (definitions.json, config).
- Default user from compose env (`RABBITMQ_USER`/`RABBITMQ_PASS`).

## Data

- Named volume `rabbitmq_data`.

## Deploy

    docker compose up -d rabbitmq

Definitions change → restart the broker; consumers re-declare with the
same arguments (a different argument set fails with PRECONDITION_FAILED).

## Verify live

    docker exec jarvis-rabbitmq rabbitmq-diagnostics -q ping
    curl -s -u <user>:<pass> 127.0.0.1:15672/api/queues | head -c 300

## Failure modes

- A 0-byte MQTT retained dets file made every boot fail (3,300+
  restarts, 2026-11-04) — the watchdog escalates this; a human deletes it.
- `x-priority` instead of `x-max-priority` made every queue plain FIFO
  (fixed 2026-11-02, PR #134).
- Publish racing a bind is dropped as NO_ROUTE (2026-11-09) — wait for
  consumers or publish mandatory.
- A reply to a non-`flow.*` queue name is confirmed and then dropped.
