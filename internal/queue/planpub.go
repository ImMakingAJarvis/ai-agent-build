// PlanPublisher publishes plan_epic jobs at the front of a priority
// queue (B1 of docs/work-board/PLAN.md). A planning job carries
// PlanEpicPriority — the maximum the background queue accepts — so it
// lines up ahead of normal background work (priority 1) in the same
// queue. Priority only orders pending deliveries: it never preempts a
// job that is already running, and the worker's scheduler still pauses
// the background queue while the conversation queues have work.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// PlanPublisher publishes planning jobs to one priority queue (normally
// BackgroundQueue). Each publish uses a dedicated channel, the same
// pattern as QueueDepth: a failed declare (e.g. the live queue still has
// the old x-max-priority) closes only that channel, never the consumer's.
type PlanPublisher struct {
	conn  *Conn
	queue string
}

// NewPlanPublisher builds a publisher for queue.
func NewPlanPublisher(conn *Conn, queue string) *PlanPublisher {
	return &PlanPublisher{conn: conn, queue: queue}
}

// PublishPlan publishes m at the front of the publisher's queue and
// returns how many jobs are queued behind it (its position minus one,
// for the board comment). It blocks until the broker confirms the
// publish, so the returned count includes the plan job itself. It
// re-declares and re-binds the queue first
// so a broker that comes up before any worker does not drop the message
// (a topic exchange drops unbound routing keys). The declare uses the
// plan priority's args on purpose: a queue that accepts plan jobs is a
// priority queue, and if the live queue was declared with different
// args RabbitMQ fails the channel (PRECONDITION_FAILED) and the caller
// falls back to store-only.
func (p *PlanPublisher) PublishPlan(ctx context.Context, m Message) (int, error) {
	ch, err := p.conn.NewChannel()
	if err != nil {
		return 0, fmt.Errorf("plan publish: %w", err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return 0, fmt.Errorf("plan publish: confirm mode: %w", err)
	}
	args := amqp.Table{"x-max-priority": int64(PlanEpicPriority)}
	if _, err := ch.QueueDeclare(p.queue, true, false, false, false, args); err != nil {
		return 0, fmt.Errorf("plan publish: declare %s: %w", p.queue, err)
	}
	if err := ch.QueueBind(p.queue, p.queue+".#", p.conn.Exchange(), false, nil); err != nil {
		return 0, fmt.Errorf("plan publish: bind %s: %w", p.queue, err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub, err := newPublishing(m, uint8(PlanEpicPriority))
	if err != nil {
		return 0, fmt.Errorf("plan publish: encode job %s: %w", m.ID, err)
	}
	if err := ch.PublishWithContext(ctx, p.conn.Exchange(), p.queue+".plan", false, false, pub); err != nil {
		return 0, fmt.Errorf("plan publish: %s: %w", p.queue, err)
	}
	// AMQP publishes are async: wait for the broker's confirm so the
	// depth read below counts this job, not a queue state from before
	// it was routed.
	select {
	case c := <-confirms:
		if !c.Ack {
			return 0, errors.New("plan publish: broker nacked the message")
		}
	case <-time.After(10 * time.Second):
		return 0, errors.New("plan publish: timed out waiting for confirm")
	case <-ctx.Done():
		return 0, fmt.Errorf("plan publish: waiting for confirm: %w", ctx.Err())
	}
	// depth includes the job just published; clamp against a consumer
	// draining the queue between publish and inspect.
	depth, err := p.conn.QueueDepth(p.queue)
	if err != nil {
		return 0, fmt.Errorf("plan publish: depth of %s: %w", p.queue, err)
	}
	behind := depth - 1
	if behind < 0 {
		behind = 0
	}
	return behind, nil
}
