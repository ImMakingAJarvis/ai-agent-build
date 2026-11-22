// Package kshadow is the phase-1 dual-write mirror of the RabbitMQ→Kafka
// shadow migration (docs/kafka-migration-plan.md §5): a consumer on the
// kq.mirror AMQP queue (bound "#" on both the tasks and events exchanges —
// the exchange fan-out is the tap, no publisher changes) writes every
// mirrored message to the same-named Kafka topic with header source=amqp
// and records a per-queue watermark on kq.watermark. Its per-queue
// accounting (seq/written/mismatch + the management baselines) persists to
// a state file the host can read; parity.go turns that into the parity
// report shared by `jarvis-kq parity` and the dashboard tile.
package kshadow

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// MirroredQueue is the AMQP tap queue kshadow consumes. It is declared by
// kshadow (not rabbitmq/definitions.json — it is kshadow's own, like the
// on-demand flow/cap/agent queues) and bound "#" on both exchanges, so it
// receives exactly one copy of every message published to tasks or events.
const MirroredQueue = "kq.mirror"

// WatermarkTopic receives one record per queue carrying the last mirrored
// message id (the rollback path, plan §7: on rollback the live consumers
// resume from the watermark — every message at or below it is already in
// the shadow, so the purge boundary is the watermark, not the log).
const WatermarkTopic = "kq.watermark"

// EventsQueue is the attribution key for events-exchange messages that do
// not route to a cadence queue (job.*, agent.* events, window/lease/stream
// /worker heartbeats): they are mirrored to the single "events" topic with
// the routing key kept in a header (plan §3.1 — one events topic, 30-day
// retention), never to per-event topics (a per-round agent.<id>.<state> key
// would explode the topic count).
const EventsQueue = "events"

// Attribution is where one mirrored message belongs.
type Attribution struct {
	// Exchange is "tasks" or "events".
	Exchange string
	// Queue is the source queue the message was routed to: the routing key
	// itself on tasks (queue names are the routing keys — every publisher
	// routes on the exact queue name), the cadence queue for
	// sched.<cadence>.* events, and EventsQueue for all other events.
	Queue string
}

// Static task-side queue prefixes (rabbitmq/definitions.json + the on-demand
// families). A routing key on tasks is its own queue name; these prefixes
// separate it from events keys that share a first segment (agent.*).
var taskPrefixes = []string{
	"voice", "fix", "image", "coding", "background",
	"review.", "jobs.", "flow.", "cap.", "kq.test.",
}

// attribute maps a (routing key) to its exchange and source queue. The
// delivery does not carry the source exchange, so the key's shape decides:
// keys ending in ".inbox" are mailbox queues (agent.<name>.inbox on tasks),
// keys under a task prefix are tasks, and everything else is an event
// (job.*, agent.<id>.<state>, window.*, lease.*, stream.*, worker.*,
// sched.*).
func attribute(routingKey string) Attribution {
	if strings.HasSuffix(routingKey, ".inbox") {
		return Attribution{Exchange: "tasks", Queue: routingKey}
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(routingKey, p) {
			return Attribution{Exchange: "tasks", Queue: routingKey}
		}
	}
	// sched.<cadence>.<state> → the cadence queue (bound <name>.# on
	// events); other events have no queue of their own.
	if parts := strings.SplitN(routingKey, ".", 3); parts[0] == "sched" && len(parts) >= 2 {
		return Attribution{Exchange: "events", Queue: parts[0] + "." + parts[1]}
	}
	return Attribution{Exchange: "events", Queue: EventsQueue}
}

// messageID extracts the stable identity of a mirrored message for the
// watermark (plan §7: the worker already uses the message id as the
// agent-job id, so the id is the envelope's, when present):
//
//  1. the AMQP message-id property (no current publisher sets it);
//  2. the envelope "id" (v1/v2 job messages);
//  3. the envelope "correlation_id" (capability requests/results);
//  4. the event "job_id";
//  5. a truncated content hash (last resort — stable per distinct body).
func messageID(amqpMessageID string, body []byte) string {
	if amqpMessageID != "" {
		return amqpMessageID
	}
	var probe struct {
		ID            string `json:"id"`
		CorrelationID string `json:"correlation_id"`
		JobID         string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if probe.ID != "" {
			return probe.ID
		}
		if probe.CorrelationID != "" {
			return probe.CorrelationID
		}
		if probe.JobID != "" {
			return probe.JobID
		}
	}
	sum := sha1.Sum(body)
	return "h" + hex.EncodeToString(sum[:8])
}

// MessageID is the mirror's per-message identity, exported for the
// rollback path (plan §7): jarvis-kq rollback matches the popped AMQP
// message against the last mirrored id using exactly these rules.
func MessageID(amqpMessageID string, body []byte) string {
	return messageID(amqpMessageID, body)
}

// decodeEventID is unused by the mirror (the body is copied verbatim); the
// probe above covers it. It exists so queue.DecodeEvent stays the reference
// for event bodies and this file does not open a second decoding path.
var _ = queue.DecodeEvent

// watermark is one kq.watermark record: the last mirrored message for a
// queue. Seq is kshadow's per-queue count of messages mirrored so far, so
// two messages without ids still order by seq.
type watermark struct {
	Queue     string    `json:"queue"`
	Exchange  string    `json:"exchange"`
	MessageID string    `json:"message_id"`
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
}
