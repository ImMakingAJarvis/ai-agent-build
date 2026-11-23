---
id: kshadow
name: Kafka shadow mirror
summary: Phase-1 dual-write mirror — binds "#" on the tasks and events exchanges and writes every AMQP delivery to the same-named Kafka topic (header source=amqp) with a per-queue watermark on kq.watermark; phase-2 dry-run consumer per family that logs what the Kafka side would do and gates on a 7-day match against the real AMQP events.
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

Phase 2 adds the shadow dry-run: one consumer lane per family
(events, sched, review, mailboxes, cap, flow, jobs, worker) reading the
mirrored Kafka topics and logging what the Kafka side WOULD do (deliver
job X, wake job Y, run cap Z), matched against what the AMQP side
actually did (job.*/agent.* events on the `events` topic). Match modes:
`event` (cap/jobs/worker — expect a job event with job_id = the envelope
id), `agent` (flow/mailboxes — expect an agent.<job-id>.* event; job id
from the topic name or the envelope's wake), `depth` (events/sched/review —
well-formedness only, the live drain is the mirror's Consumed column).
The worker family runs per-base-queue lanes polling `[X.pN, X]` in order
(mirroring the real worker's per-queue goroutines, §3.2) and counts
priority/FIFO inversions within a 5-minute concurrency window. A janitor
sweep (30s) counts would-expire (TTL) entries (§3.3) and compares dead
queue arrivals (mgmt Ready+Unacked+Delivered minus the first-sweep
baseline) against them. The 7-day gate: exit 0 only when no family has an
unmatched would-do or an inversion.

## Interfaces

- Consumes `kq.mirror` (durable, manual ack, Qos 1, no DLX by design — a
  message that cannot be mirrored waits on the mirror queue instead of
  leaking into dead).
- Produces to the per-queue topics, `events` and `kq.watermark` on Kafka
  (kafka:9093).
- State file `/data/kshadow-state.json` (bind-mounted from the host) —
  per-queue seq/written/mismatch/last-id/baselines; read by `jarvis-kq
  parity` and the dashboard tile.
- Dry-run state file `/data/kshadow-dryrun.json` (same mount) — per-topic
  offsets, per-family counters, pending/stale would-do entries, seen-id
  set (idempotence, §7.2); read by `jarvis-kq parity` and the dashboard
  tile's dry-run gate line.
- No port of its own; the health probe targets the tap queue via the
  RabbitMQ management API (the queue exists once kshadow has started).

## Configuration

- `RABBITMQ_URL` — compose env, `amqp://jarvis:jarvis-local@rabbitmq:5672`;
  the management base URL is derived from it (host:15672) unless
  `KSHADOW_RABBITMQ_MGMT` is set.
- `KAFKA_BROKERS` — compose env, `kafka:9093`.
- `KSHADOW_STATE` — compose env, `/data/kshadow-state.json` (host:
  `~/.local/share/jarvis/kshadow/kshadow-state.json`).
- `KSHADOW_DRYRUN_STATE` — optional; defaults to `kshadow-dryrun.json`
  next to `KSHADOW_STATE` (host:
  `~/.local/share/jarvis/kshadow/kshadow-dryrun.json`).

## Data

- `~/.local/share/jarvis/kshadow/kshadow-state.json` — the accounting;
  atomic temp+rename saves, throttled to ~2s. Corrupt file = hard error at
  startup (do not delete it to "fix" a restart; the watermarks on
  kq.watermark are the recovery source).
- `~/.local/share/jarvis/kshadow/kshadow-dryrun.json` — the dry-run
  accounting; missing file = fresh state (first run), corrupt file = hard
  error. Deleting it restarts the 7-day gate (readers return to log end,
  counters zero).
- Kafka topics: one per mirrored queue (auto-created on first write),
  `events`, `kq.watermark`.

## Deploy

    docker compose build kshadow && docker compose up -d kshadow

Confirm: `docker exec jarvis-kshadow test -s /data/kshadow-state.json`
within a couple of minutes of the first tasks/events traffic.

## Verify live

    jarvis-kq parity                      # exit 0 = zero mismatch (parity + dry-run), events lag < 5m
    jarvis-kq parity --json | head -c 400
    curl -s http://111.1.2.27:8080/html/kafka-shadow | head -c 300
    docker exec jarvis-kafka /opt/bitnami/kafka/bin/kafka-consumer.sh --bootstrap-server 127.0.0.1:9092 --from-beginning --max-messages 1 --property print.properties=true kq.watermark
    test -s ~/.local/share/jarvis/kshadow/kshadow-dryrun.json   # dry-run started
    docker logs jarvis-kshadow 2>&1 | grep 'dry-run' | tail -5  # would-do notes

## Failure modes

- Kafka down: writes retry 6×5s per message, then the delivery is Nacked
  and requeued — the message accumulates on kq.mirror (visible as the
  ready depth climbing in parity), never lost, and is mirrored when the
  broker returns. The live AMQP path is unaffected: the tap is a
  separate queue.
- RabbitMQ management API unreachable: baselines cannot be taken or
  refreshed; parity falls back to the tap's own seq and renders consumed
  as "-".
- The logical `events` aggregate has no AMQP queue of that name: its
  mgmt lookup 404s and kshadow backs off 60s between retries (no
  per-message retries). Parity renders CONSUMED "-" there — the tap's
  seq is the aggregate's published count.
- A broker restart resets the management counters below the captured
  baselines; parity then reads the window "since the restart" (published
  may show as 0 or negative → "-"). Reboot kshadow after a broker restart
  to re-baseline.
- The 48h acceptance gate (zero mismatch, bounded shadow lag) runs over
  `jarvis-kq parity` exit codes — see docs/kafka-migration-plan.md §5.
- Dry-run restart: per-topic offsets, pending/stale entries and the
  seen-id set are persisted, so a container restart neither re-matches
  old would-dos nor misses new ones (replayed records dedupe by message
  id, §7.2). An offset past the log bounds (post-purge) is dropped: a
  lane topic is recreated at the log end (purged records could never
  match — their events were purged too) and the `events` topic re-reads
  from FirstOffset (matching is idempotent per message id).
- A dry-run reader error (Kafka blip) backs off 2s and re-reads; the
  mirror side is unaffected and vice versa — one side exiting takes down
  the container, which restarts both.
- kafka-go v0.4.51 applies `ReaderConfig.StartOffset` only to
  consumer-group readers; a plain reader silently starts at FirstOffset
  (log replay on every restart). The dry-run positions each reader with
  `SetOffset` before its first read (LastOffset = log end = new messages
  only) — keep it that way.
