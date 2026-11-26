// The loop.events queue carries runtime events (ELR-1, M1.3a, card
// ELR-13): a tool finished, a user sent a message, an approval was
// decided. Unlike the fire-and-forget lifecycle events on the "events"
// exchange (events.go), loop.events is a durable, dead-lettered queue —
// a runtime event is state the loop must see, so it is held on the
// broker until consumed and, if it is ever rejected, dead-letters to the
// dead queue with a reason instead of vanishing.
//
// The queue is created on demand (EnsureLoopEventsQueue) exactly like a
// flow queue (flow.go): it also appears in rabbitmq/definitions.json so
// a broker reloaded from definitions has it too; the code path works
// before a reload as well.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LoopEventsQueue is the durable queue that carries runtime events.
const LoopEventsQueue = "loop.events"

// LoopEvent is one runtime event. Type is one of the Ev* constants;
// JobID/OpID name the task and operation the event is about (either may
// be empty), and Payload is opaque JSON for the event's type.
type LoopEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	JobID   string          `json:"job_id,omitempty"`
	OpID    string          `json:"op_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	At      time.Time       `json:"at"`
}

// Event type constants.
const (
	EvTaskSubmitted     = "task.submitted"
	EvTaskReady         = "task.ready"
	EvTaskDone          = "task.done"
	EvOpStarted         = "op.started"
	EvOpProgress        = "op.progress"
	EvOpDone            = "op.done"
	EvApprovalRequested = "approval.requested"
	EvApprovalDecided   = "approval.decided"
	EvUserMessage       = "user.message"
	EvTimerFired        = "timer.fired"
	EvModelSuspended    = "model.suspended"
	EvModelResumed      = "model.resumed"
	EvCancelRequested   = "cancel.requested"
)

// NewLoopEvent builds an event of type typ for job jobID. The id is
// "ev-<unix nanos>-<6 random hex>" (unique across the fleet: the nanosecond
// stamp separates calls and the random tail separates calls in the same
// nanosecond), and At is now in UTC.
func NewLoopEvent(typ, jobID string) LoopEvent {
	now := time.Now()
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable mid-process; a panic is
		// the honest answer (the id would otherwise collide).
		panic(fmt.Sprintf("queue: crypto/rand: %v", err))
	}
	return LoopEvent{
		ID:    fmt.Sprintf("ev-%d-%s", now.UnixNano(), hex.EncodeToString(b)),
		Type:  typ,
		JobID: jobID,
		At:    now.UTC(),
	}
}

// EnsureLoopEventsQueue declares the loop.events queue (durable, with the
// dead-letter args — see dlx.go) and binds it to the tasks exchange on
// loop.events.#, so a message published before any consumer exists is
// held on the broker until it is consumed or dead-letters with a reason.
// Re-declaring with identical args is a no-op, which is what makes this
// safe to call on every publish; a queue declared with different args
// (for instance without the dead-letter arg) is migrated by
// declareWithDLX when it holds nothing.
func (c *Conn) EnsureLoopEventsQueue() error {
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("loop.events: %w", err)
	}
	if err := c.declareWithDLX(LoopEventsQueue); err != nil {
		return fmt.Errorf("loop.events: declare: %w", err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("loop.events: %w", err)
	}
	defer ch.Close()
	// Synchronous bind — see EnsureFlowQueue: a noWait bind returns
	// before the broker has registered the binding, and a publish on
	// another channel of the same connection can race ahead of it.
	if err := ch.QueueBind(LoopEventsQueue, "loop.events.#", c.Exchange(), false, nil); err != nil {
		return fmt.Errorf("loop.events: bind: %w", err)
	}
	return nil
}

// PublishLoopEvent publishes ev to the loop.events queue, creating and
// binding it first, and blocks until the broker confirms the publish
// (persistent delivery in confirm mode, exactly as PublishFlow). The
// routing key is "loop.events.default" and the AMQP message id is ev.ID.
func (c *Conn) PublishLoopEvent(ctx context.Context, ev LoopEvent) error {
	if err := c.EnsureLoopEventsQueue(); err != nil {
		return err
	}
	body, err := EncodeLoopEvent(ev)
	if err != nil {
		return fmt.Errorf("loop.events: encode event %s: %w", ev.ID, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("loop.events: %w", err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("loop.events: confirm mode: %w", err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub := amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    ev.At,
		MessageId:    ev.ID,
	}
	if err := ch.PublishWithContext(ctx, c.Exchange(), "loop.events.default", false, false, pub); err != nil {
		return fmt.Errorf("loop.events: publish: %w", err)
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return fmt.Errorf("loop.events: broker nacked the message")
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("loop.events: timed out waiting for confirm")
	case <-ctx.Done():
		return fmt.Errorf("loop.events: waiting for confirm: %w", ctx.Err())
	}
	return nil
}

// EncodeLoopEvent serializes ev to the JSON body published to
// loop.events.
func EncodeLoopEvent(ev LoopEvent) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal LoopEvent: %w", err)
	}
	return b, nil
}

// DecodeLoopEvent parses a loop.events body. It rejects a body with an
// empty ID or Type: an event with no id cannot be deduplicated or
// correlated, and one with no type cannot be routed to a handler.
func DecodeLoopEvent(b []byte) (LoopEvent, error) {
	var ev LoopEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return LoopEvent{}, fmt.Errorf("unmarshal LoopEvent: %w", err)
	}
	if ev.ID == "" {
		return LoopEvent{}, fmt.Errorf("loop.events: event has no id")
	}
	if ev.Type == "" {
		return LoopEvent{}, fmt.Errorf("loop.events: event %s has no type", ev.ID)
	}
	return ev, nil
}

// ConsumeLoopEvents consumes loop.events with manual ack and prefetch 1
// (ELR-1, M1.3b): the delivery is acked only after the handler has saved
// the event's effect. For each delivery it decodes the event — a body
// that does not decode is rejected without requeue (it dead-letters with
// the reason; requeuing it would loop forever on the same bad body) —
// and calls handle. When handle returns nil the delivery is acked; when
// it returns an error the shared delivery budget (dlx.go) decides, the
// same way the cap worker does (capworker.go): under MaxAttempts the
// event is republished with the x-attempts header bumped — the copy is
// confirmed by the broker before the failed delivery is acked, so the
// event cannot be lost — and at the budget the delivery is rejected
// without requeue, which dead-letters it (reason "rejected").
//
// The handler, not this function, is responsible for calling
// Store.EventApplied first and Store.MarkEventApplied last: the mark is
// durable before the ack, so a redelivery of an already-applied event is
// a no-op — the consumer never loses an event and never applies one
// twice. It returns when ctx is done (nil), or when the delivery channel
// closes (an error: the queue is durable, the caller reconnects).
func (c *Conn) ConsumeLoopEvents(ctx context.Context, handle func(LoopEvent) error) error {
	if err := c.EnsureLoopEventsQueue(); err != nil {
		return err
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("loop.events: consume: %w", err)
	}
	defer ch.Close()
	if err := ch.Qos(1, 0, true); err != nil {
		return fmt.Errorf("loop.events: qos: %w", err)
	}
	tag := fmt.Sprintf("loop-events-%d", time.Now().UnixNano())
	dels, err := ch.Consume(LoopEventsQueue, tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("loop.events: consume: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case del, ok := <-dels:
			if !ok {
				return errors.New("loop.events: delivery channel closed")
			}
			c.handleLoopEvent(ctx, del, handle)
		}
	}
}

// handleLoopEvent applies the delivery contract to one loop.events
// message: decode (an undecodable body is rejected without requeue), then
// the handler — nil acks the delivery, an error burns an attempt.
func (c *Conn) handleLoopEvent(ctx context.Context, del amqp.Delivery, handle func(LoopEvent) error) {
	ev, err := DecodeLoopEvent(del.Body)
	if err != nil {
		del.Nack(false, false) // undecodable: reject without requeue -> DLX
		return
	}
	if err := handle(ev); err != nil {
		c.retryOrDeadLetter(ctx, del, err)
		return
	}
	del.Ack(false)
}

// retryOrDeadLetter burns an attempt on a handler error (the shared
// delivery budget, dlx.go). Under the budget: republish the event with
// the bumped x-attempts header — the copy is confirmed by the broker
// before the failed delivery is acked, so the event cannot be lost. At
// the budget: nack without requeue, which dead-letters the message
// (reason "rejected"). A republish that cannot be confirmed is requeued,
// exactly as the cap worker's retry does.
func (c *Conn) retryOrDeadLetter(ctx context.Context, del amqp.Delivery, herr error) {
	n := AttemptCount(del) + 1
	if n >= MaxAttempts {
		del.Nack(false, false)
		return
	}
	ch, err := c.NewChannel()
	if err != nil {
		// Leave the delivery unacked: the channel close requeues it on
		// the broker and the event is retried.
		return
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub := amqp.Publishing{
		Body:          del.Body,
		ContentType:   del.ContentType,
		CorrelationId: del.CorrelationId,
		ReplyTo:       del.ReplyTo,
		DeliveryMode:  amqp.Persistent,
		Timestamp:     time.Now(),
		Headers:       BumpAttempt(del),
	}
	// The queue is bound to the tasks exchange on loop.events.# (see
	// EnsureLoopEventsQueue); .default is the key PublishLoopEvent uses.
	routingKey := LoopEventsQueue + ".default"
	if err := ch.PublishWithContext(ctx, c.Exchange(), routingKey, false, false, pub); err != nil {
		del.Nack(false, true)
		return
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			del.Nack(false, true)
			return
		}
	case <-time.After(10 * time.Second):
		del.Nack(false, true)
		return
	case <-ctx.Done():
		return // leaving it unacked; the channel close requeues it
	}
	del.Ack(false)
}
