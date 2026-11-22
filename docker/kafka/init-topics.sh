#!/bin/bash
# Creates the static topic set for the RabbitMQ→Kafka shadow migration
# (docs/kafka-migration-plan.md §3.2). Idempotent: --if-not-exists, so it
# can run on every broker start. Topic names and retention come from the
# plan: per-priority families get a base topic plus one per live priority;
# dead, events and kq.watermark keep 30 days, everything else 7 days;
# single partition, min.insync.replicas=1.
set -euo pipefail

# In-container clients use the PLAINTEXT listener (kafka:9093); the host
# port map 127.0.0.1:9092 serves the EXTERNAL listener for host clients.
BOOTSTRAP="${KAFKA_BOOTSTRAP:-kafka:9093}"
TOPICS_SH=/opt/bitnami/kafka/bin/kafka-topics.sh

RETENTION_7D=604800000    # 7 days, plan §3.2
RETENTION_30D=2592000000  # 30 days for dead + events

# topic retention_ms
declare -a TOPICS=(
    # worker queues: base + per-priority family
    "voice $RETENTION_7D"
    "voice.p10 $RETENTION_7D"
    "fix $RETENTION_7D"
    "image $RETENTION_7D"
    "image.p10 $RETENTION_7D"
    "coding $RETENTION_7D"
    "coding.p5 $RETENTION_7D"
    "background $RETENTION_7D"
    "background.p10 $RETENTION_7D"
    # reviewer inboxes
    "review.sonnet $RETENTION_7D"
    "review.opus $RETENTION_7D"
    "review.fable $RETENTION_7D"
    "review.local $RETENTION_7D"
    # capability/location queues
    "jobs.gpu $RETENTION_7D"
    "jobs.cpu $RETENTION_7D"
    "jobs.igpu $RETENTION_7D"
    # cadence queues
    "sched.hourly $RETENTION_7D"
    "sched.daily $RETENTION_7D"
    "sched.weekly $RETENTION_7D"
    "sched.monthly $RETENTION_7D"
    # dead letters + events: 30 days
    "dead $RETENTION_30D"
    "events $RETENTION_30D"
    # shadow watermarks (one per mirrored queue, keyed by queue): 30 days —
    # the phase-4 rollback path (plan §7) replays from them
    "kq.watermark $RETENTION_30D"
    # group control log (pause/resume/throttle/rebalance, plan §4): 30 days —
    # a control sticks while the topic retains it
    "kq.control $RETENTION_30D"
    # dead-drop markers (offset of each dropped dead record): 30 days,
    # matching the dead topic so a marker never outlives its record
    "kq.dead.dropped $RETENTION_30D"
)

for entry in "${TOPICS[@]}"; do
    set -- $entry
    topic=$1
    retention=$2
    "$TOPICS_SH" --bootstrap-server "$BOOTSTRAP" \
        --create --if-not-exists \
        --topic "$topic" \
        --partitions 1 \
        --config "retention.ms=${retention}" \
        --config "min.insync.replicas=1"
    echo "topic ${topic} ready (retention ${retention}ms)"
done

echo "kafka-init: ${#TOPICS[@]} topics ensured on ${BOOTSTRAP}"
