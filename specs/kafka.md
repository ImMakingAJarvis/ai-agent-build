---
id: kafka
name: Kafka broker
summary: KRaft single-node Kafka for the RabbitMQ→Kafka shadow migration — the topic set that mirrors the AMQP queues, plus the in-repo Prometheus exporter and the jarvis-kq CLI.
group: plumbing
kind: container
status: active
compose: [kafka, kafka-init, kafka-exporter]
binaries: [cmd/jarvis-kq, cmd/kafka-exporter]
ports: ["9092 Kafka EXTERNAL (127.0.0.1)", "9093 Kafka PLAINTEXT (compose network)", "9094 KRaft controller (compose network)", "9308 exporter (127.0.0.1)"]
health: tcp://kafka:9092
docs: [docs/kafka-migration-plan.md]
depends_on: []
---

## Purpose

The target broker of the RabbitMQ→Kafka migration
(docs/kafka-migration-plan.md). Phase 0 runs it alongside RabbitMQ with the
static topic set that mirrors the AMQP queues: worker queues with
per-priority families (voice/.p10, image/.p10, coding/.p5, background/.p10),
reviewer inboxes (review.*), capability queues (jobs.gpu/cpu/igpu), cadence
queues (sched.*), and dead, events and kq.watermark with 30-day retention
(everything else 7-day). kq.watermark carries the phase-1 shadow watermarks
(sees kshadow). `jarvis-kq` is the inspection/modify CLI (topics, depth,
peek, tail, groups, lag, history, describe, send, purge, drop, parity);
`kafka-exporter` feeds Prometheus.

## Interfaces

- Kafka — two client listeners: EXTERNAL on 127.0.0.1:9092 (host clients,
  advertised as `127.0.0.1:9092`) and PLAINTEXT on kafka:9093 (in-container
  clients, advertised as `kafka:9093`). Kafka clients follow the advertised
  host for partition leaders and group queries, so one name cannot serve
  both sides of the compose boundary. KRaft controller port 9094 is
  internal, never a client path.
- Exporter on :9308 — `/metrics` (kafka_* gauges) and `/health`.
- `jarvis-kq` CLI (host), broker list from `KAFKA_BROKERS`.

## Configuration

- Compose env (KAFKA_CFG_*): KRaft single node (node 0, broker+controller,
  controller on :9094), listeners PLAINTEXT :9093 + EXTERNAL :9092
  (advertised kafka:9093 / 127.0.0.1:9092), `auto.create.topics.enable=true`,
  `delete.topic.enable=true`, `log.retention.hours=168` (7d default),
  `min.insync.replicas=1`, `num.partitions=1`, 1g heap.
- `docker/kafka/init-topics.sh` (kafka-init, one-shot after the broker is
  healthy): creates the 23 static topics with per-topic retention
  (30d for dead/events/kq.watermark) — idempotent.
- Exporter env: `KAFKA_BROKERS`, `LISTEN_ADDR`, `POLL_INTERVAL`.

## Data

- Named volume `kafka_data` at /bitnami/kafka (KRaft logs + metadata).

## Deploy

    docker compose build kafka-exporter && docker compose up -d kafka kafka-init kafka-exporter

The image is `bitnamilegacy/kafka:3.7`: bitnami moved its Docker Hub org
in 2025 and the old `bitnami/kafka` name no longer resolves — same image.

## Verify live

    docker exec jarvis-kafka /opt/bitnami/kafka/bin/kafka-topics.sh --bootstrap-server 127.0.0.1:9092 --list | head
    jarvis-kq topics
    curl -s 127.0.0.1:9308/metrics | grep kafka_topic_partition_depth
    curl -s '127.0.0.1:9090/api/v1/query?query=kafka_topic_partition_depth' | head -c 200

## Failure modes

- `bitnami/kafka:3.7` and the usual exporter images (danielmtaylor, dpkp,
  landoop) are pull-denied on this box — hence the bitnamilegacy tag and
  the in-repo exporter.
- A recreate of the broker keeps `kafka_data`; the KRaft node id (0) must
  stay stable or the broker fails to join its own quorum.
- A client on the wrong side of the boundary (host using kafka:9093,
  container using 127.0.0.1:9092) succeeds on bootstrap metadata, then
  fails on leader and group dials — the advertised host it receives is
  unreachable where the client runs. Host: 127.0.0.1:9092; containers:
  kafka:9093.
