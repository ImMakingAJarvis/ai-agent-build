// LocationPublisher publishes execution jobs onto the location queues
// (jobs.gpu / jobs.cpu / jobs.igpu, A6). The bridge (C1) places a run_task
// job with placement.Decide and publishes it here; the host task-runner's
// LocationWorkers consume it. Each publish uses a dedicated channel, the
// same pattern as PlanPublisher: a failed declare closes only that
// channel, never the consumer's.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LocationPublisher publishes execution jobs to the per-location queues.
type LocationPublisher struct {
	conn *Conn
}

// NewLocationPublisher builds a publisher for the location queues.
func NewLocationPublisher(conn *Conn) *LocationPublisher {
	return &LocationPublisher{conn: conn}
}

// PublishLocation publishes m to the queue for loc ("gpu", "cpu" or
// "igpu") and blocks until the broker confirms, so a nil error means the
// message is durable. A location with no box queue — "cloud" runs on the
// provider's hardware — is an error, not a drop.
func (p *LocationPublisher) PublishLocation(ctx context.Context, loc string, m Message) error {
	q, ok := LocationQueueFor(loc)
	if !ok {
		return fmt.Errorf("location publish: location %q has no box queue", loc)
	}
	return p.publish(ctx, q, m)
}

// publish is the queue-named core of PublishLocation; the tests drive it
// with scratch queues.
func (p *LocationPublisher) publish(ctx context.Context, q string, m Message) error {
	ch, err := p.conn.NewChannel()
	if err != nil {
		return fmt.Errorf("location publish: %w", err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("location publish: confirm mode: %w", err)
	}
	// Plain declare, nil args: LocationWorker.Start declares these queues
	// without x-max-priority, and a redeclare with different args fails
	// the channel (PRECONDITION_FAILED) — the same trap as the plan
	// queue's priority migration (B1).
	if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
		return fmt.Errorf("location publish: declare %s: %w", q, err)
	}
	if err := ch.QueueBind(q, q+".#", p.conn.Exchange(), false, nil); err != nil {
		return fmt.Errorf("location publish: bind %s: %w", q, err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub, err := newPublishing(m, 0)
	if err != nil {
		return fmt.Errorf("location publish: encode job %s: %w", m.ID, err)
	}
	// Routing key is the queue name itself: the q".#" topic binding
	// matches it, and the location queues carry no priority, so no wire
	// priority is set either.
	if err := ch.PublishWithContext(ctx, p.conn.Exchange(), q, false, false, pub); err != nil {
		return fmt.Errorf("location publish: %s: %w", q, err)
	}
	// AMQP publishes are async: wait for the broker's confirm so a nil
	// return means the message is durably enqueued.
	select {
	case c := <-confirms:
		if !c.Ack {
			return errors.New("location publish: broker nacked the message")
		}
	case <-time.After(10 * time.Second):
		return errors.New("location publish: timed out waiting for confirm")
	case <-ctx.Done():
		return fmt.Errorf("location publish: waiting for confirm: %w", ctx.Err())
	}
	return nil
}
