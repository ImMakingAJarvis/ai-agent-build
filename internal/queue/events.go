// Lifecycle events on the events exchange (plan §3.4).
//
// Everything that reacts to what the stack is doing — the dashboard, the
// Grafana exporter, the Discord notifier, the switchboard — subscribes to
// the durable topic exchange "events". Routing-key families:
//
//	job.<capability>.<started|progress|done|failed>
//	agent.<job-id>.<yielded|woken|resumed>
//	window.<opened|closed>
//	lease.<granted|released|denied>
//	worker.<worker-id>.<heartbeat|loaded|unloaded>
//
// Events are fire-and-forget and never the source of truth: a lost event
// is acceptable, so publishing one must never fail or block the emitting
// job (Publish awaits no broker confirm, and callers log, not abort, on
// error).
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// EventsExchange is the topic exchange lifecycle events are published to.
const EventsExchange = "events"

// Event kinds — the first routing-key segment.
const (
	JobKind    = "job"    // job.<capability>.<state>
	AgentKind  = "agent"  // agent.<job-id>.<state>
	WindowKind = "window" // window.<state>
	LeaseKind  = "lease"  // lease.<state>
	WorkerKind = "worker" // worker.<worker-id>.<state>
	SchedKind  = "sched"  // sched.<cadence>.<state>
	StreamKind = "stream" // stream.<state> — the game/stream watchdog (EVB-29)
)

// Event is one lifecycle event. Kind/Subject/State are the routing key's
// segments; the rest is payload for subscribers.
type Event struct {
	Kind    string `json:"kind"` // job | agent | window | lease
	Subject string `json:"subject,omitempty"`
	State   string         `json:"state"`
	JobID   string         `json:"job_id,omitempty"`
	Source  string         `json:"source,omitempty"` // worker, task-runner, agent-run, ...
	At      time.Time      `json:"at"`
	Data    map[string]any `json:"data,omitempty"`
}

// RoutingKey is the topic routing key. job.* and agent.* carry a subject
// (the capability or job id); window.* and lease.* do not.
func (e Event) RoutingKey() string {
	if e.Subject == "" {
		return e.Kind + "." + e.State
	}
	return e.Kind + "." + e.Subject + "." + e.State
}

// JobEvent builds a job.<capability>.<state> event for a message: the
// capability is the v2 Capability when set, else the v1 message type
// (voice/coding/...), so both envelopes produce the job.* family.
func JobEvent(m Message, state string, data map[string]any) Event {
	cap := m.Capability
	if cap == "" {
		cap = m.Type
	}
	return JobEventWith(m, cap, state, data)
}

// JobEventWith is JobEvent with the capability overridden — the
// task-runner's run_task messages carry the lane queue in Type
// (jobs.gpu/...), not a capability.
func JobEventWith(m Message, capability, state string, data map[string]any) Event {
	return Event{Kind: JobKind, Subject: capability, State: state, JobID: m.ID, At: time.Now().UTC(), Data: data}
}

// AgentEvent builds an agent.<job-id>.<state> event (yielded/woken/resumed
// for a brain job's round lifecycle).
func AgentEvent(jobID, state string, data map[string]any) Event {
	return Event{Kind: AgentKind, Subject: jobID, State: state, JobID: jobID, At: time.Now().UTC(), Data: data}
}

// WindowEvent builds a window.<state> event (a swap window opened/closed).
func WindowEvent(state string, data map[string]any) Event {
	return Event{Kind: WindowKind, State: state, At: time.Now().UTC(), Data: data}
}

// LeaseEvent builds a lease.<state> event (granted/released/denied).
func LeaseEvent(state string, data map[string]any) Event {
	return Event{Kind: LeaseKind, State: state, At: time.Now().UTC(), Data: data}
}

// StreamEvent builds a stream.<state> event (started/stopped — the
// game/stream watchdog, EVB-29). Data carries which stream ("sunshine"
// / "steam-remote-play") and, for started, what it did (revoked leases,
// window closed).
func StreamEvent(state string, data map[string]any) Event {
	return Event{Kind: StreamKind, State: state, At: time.Now().UTC(), Data: data}
}

// WorkerEvent builds a worker.<worker-id>.<state> event (the capworker
// SDK's heartbeat and loaded/unloaded transitions). The worker id may
// contain "/" (e.g. "echo@cpu/cap-echo") — only "." is special in a
// routing key.
func WorkerEvent(workerID, state string, data map[string]any) Event {
	return Event{Kind: WorkerKind, Subject: workerID, State: state, At: time.Now().UTC(), Data: data}
}

// ParseEventRoutingKey splits a routing key into its kind, subject and
// state segments. Only window.*, lease.* and stream.* keys have no
// subject; job.* and agent.* need all three segments.
func ParseEventRoutingKey(key string) (kind, subject, state string, err error) {
	parts := strings.Split(key, ".")
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2], nil
	case 2:
		if parts[0] != WindowKind && parts[0] != LeaseKind && parts[0] != StreamKind {
			return "", "", "", fmt.Errorf("events: routing key %q: %s events need a subject (job.<cap>.<state> or agent.<job-id>.<state>)", key, parts[0])
		}
		return parts[0], "", parts[1], nil
	}
	return "", "", "", fmt.Errorf("events: routing key %q: want job.<cap>.<state>, agent.<job-id>.<state>, window.<state>, lease.<state> or stream.<state>", key)
}

// DecodeEvent parses an event body (for subscribers and jarvis-mq tail).
func DecodeEvent(body []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return Event{}, fmt.Errorf("unmarshal Event: %w", err)
	}
	return e, nil
}

// EventPublisher publishes lifecycle events to the events exchange on a
// dedicated channel. It is created once per long-lived service (worker,
// task-runner) and one event's failure never reaches the service's job
// path.
type EventPublisher struct {
	conn   *Conn
	source string
	mu     sync.Mutex
	ch     *amqp.Channel
}

// NewEventPublisher opens a dedicated channel and declares the events
// exchange (idempotent — the broker also gets it from definitions.json,
// and a code path that runs before a broker reload still works). source
// names the emitting component and is stamped on events that leave it
// empty.
func NewEventPublisher(conn *Conn, source string) (*EventPublisher, error) {
	p := &EventPublisher{conn: conn, source: source}
	if err := p.open(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *EventPublisher) open() error {
	ch, err := p.conn.NewChannel()
	if err != nil {
		return fmt.Errorf("events: channel: %w", err)
	}
	if err := ch.ExchangeDeclare(EventsExchange, "topic", true, false, false, false, nil); err != nil {
		ch.Close()
		return fmt.Errorf("events: declare exchange: %w", err)
	}
	p.ch = ch
	return nil
}

// Publish sends e to the events exchange. Fire-and-forget: no broker
// confirm is awaited. On a failed publish it tries one reconnect (a broker
// restart must not silence the publisher for the rest of the process's
// life) and returns the error for the caller to log.
func (p *EventPublisher) Publish(ctx context.Context, e Event) error {
	if e.Source == "" {
		e.Source = p.source
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("events: marshal: %w", err)
	}
	pub := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.publishLocked(ctx, e, pub); err != nil {
		err = p.republishAfterReconnect(ctx, e, pub, err)
	}
	return err
}

func (p *EventPublisher) publishLocked(ctx context.Context, e Event, pub amqp.Publishing) error {
	if p.ch == nil || p.ch.IsClosed() {
		return fmt.Errorf("events: no channel")
	}
	return p.ch.PublishWithContext(ctx, EventsExchange, e.RoutingKey(), false, false, pub)
}

// republishAfterReconnect re-establishes the channel (when the broker went
// away) and retries the publish once. Called with p.mu held; the first
// error is returned while the broker is still down.
func (p *EventPublisher) republishAfterReconnect(ctx context.Context, e Event, pub amqp.Publishing, firstErr error) error {
	if !p.conn.Dead() {
		return firstErr
	}
	if err := p.conn.Reconnect(); err != nil {
		return firstErr
	}
	ch, err := p.conn.NewChannel()
	if err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(EventsExchange, "topic", true, false, false, false, nil); err != nil {
		ch.Close()
		return err
	}
	p.ch = ch
	return p.publishLocked(ctx, e, pub)
}

// Close shuts the publisher's channel down.
func (p *EventPublisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch != nil {
		p.ch.Close()
		p.ch = nil
	}
}
