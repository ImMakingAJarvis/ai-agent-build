---
id: kshadow
name: Kafka shadow mirror
summary: Phase-1 dual-write mirror — binds "#" on the tasks and events exchanges and writes every AMQP delivery to the same-named Kafka topic (header source=amqp) with a per-queue watermark on kq.watermark.
group: plumbing
kind: container
status: active
compose: [kshadow]
units: []
binaries: [cmd/kshadow]
ports: []
health: "http://127.0.0.1:15672/api/queues/%2F/kq.mirror"
docs: [docs/kafka-migration-plan.md]
depends_on:
  - to: rabbitmq
    why: "consumes the kq.mirror tap queue bound # on tasks + events"
  - to: kafka
    why: "writes the mirrored records and the kq.watermark watermarks"
  - to: dashboard
    why: "serves the Kafka shadow tile from kshadow's state file (read-only)"
---

## Purpose

The phase-1 dual-write mirror of the RabbitMQ→Kafka migration
(docs/kafka-migration-plan.md §5). Exchange fan-out is the tap: `kq.mirror`
is bound `"#"` on the `tasks` and `events` exchanges, so every published
message is delivered to the mirror without touching any publisher or the
live AMQP consumer path (they sit upstream of the tap). kshadow writes each
delivery verbatim to Kafka — tasks queues to a topic named after the queue,
events to the single `events` topic (routing key kept in the
`amqp-routing-key` header) — with headers `source=amqp`, `amqp-exchange`,
`amqp-routing-key`, `amqp-message-id`, `seq`, `ts`. It also records a
per-queue watermark (last mirrored message id + seq) on topic
`kq.watermark`, throttled to one write per 5s per queue, which is the
rollback path for plan §7.

Parity — the acceptance gate — compares three independent counts per
queue: the AMQP broker counters (management API, since kshadow's first
sight of the queue), the `source=amqp` records scanned on the Kafka topic,
and kshadow's own write count. `jarvis-kq parity` (CLI) and the dashboard's
Kafka shadow tile render the same report.

## Interfaces

- Consumes `kq.mirror` (durable, manual ack, Qos 1, no DLX by design — a
  message that cannot be mirrored waits on the mirror queue instead of
  leaking into dead).
- Produces to the per-queue topics, `events` and `kq.watermark` on Kafka
  (kafka:9093).
- State file `/data/kshadow-state.json` (bind-mounted from the host) —
  per-queue seq/written/mismatch/last-id/baselines; read by `jarvis-kq
  parity` and the dashboard tile.
- No port of its own; the health probe targets the tap queue via the
  RabbitMQ management API (the queue exists once kshadow has started).

## Configuration

- `RABBITMQ_URL` — compose env, `amqp://jarvis:jarvis-local@rabbitmq:5672`;
  the management base URL is derived from it (host:15672) unless
  `KSHADOW_RABBITMQ_MGMT` is set.
- `KAFKA_BROKERS` — compose env, `kafka:9093`.
- `KSHADOW_STATE` — compose env, `/data/kshadow-state.json` (host:
  `~/.local/share/jarvis/kshadow/kshadow-state.json`).

## Data

- `~/.local/share/jarvis/kshadow/kshadow-state.json` — the accounting;
  atomic temp+rename saves, throttled to ~2s. Corrupt file = hard error at
  startup (do not delete it to "fix" a restart; the watermarks on
  kq.watermark are the recovery source).
- Kafka topics: one per mirrored queue (auto-created on first write),
  `events`, `kq.watermark`.

## Deploy

    docker compose build kshadow && docker compose up -d kshadow

Confirm: `docker exec jarvis-kshadow test -s /data/kshadow-state.json`
within a couple of minutes of the first tasks/events traffic.

## Verify live

    jarvis-kq parity                      # exit 0 = zero mismatch, lag < 5m
    jarvis-kq parity --json | head -c 400
    curl -s http://111.1.2.27:8080/html/kafka-shadow | head -c 300
    docker exec jarvis-kafka /opt/bitnami/kafka/bin/kafka-consumer.sh --bootstrap-server 127.0.0.1:9092 --from-beginning --max-messages 1 --property print.properties=true kq.watermark

## Failure modes

- Kafka down: writes retry 6×5s per message, then the delivery is Nacked
  and requeued — the message accumulates on kq.mirror (visible as the
  ready depth climbing in parity), never lost, and is mirrored when the
  broker returns. The live AMQP path is unaffected: the tap is a
  separate queue.
- RabbitMQ management API unreachable: baselines cannot be taken or
  refreshed; parity falls back to the tap's own seq and renders consumed
  as "-".
- A broker restart resets the management counters below the captured
  baselines; parity then reads the window "since the restart" (published
  may show as 0 or negative → "-"). Reboot kshadow after a broker restart
  to re-baseline.
- The 48h acceptance gate (zero mismatch, bounded shadow lag) runs over
  `jarvis-kq parity` exit codes — see docs/kafka-migration-plan.md §5.
