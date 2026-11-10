// Flow queues are the event-driven handoff between models (see
// docs/event-driven.md). The local brain (or any round) publishes a job
// to a flow queue named flow.<name>, finishes the round, and another
// model's worker picks it up. When that work is done it publishes the
// result back to the same flow queue and adds a small agent job that
// wakes the brain; the brain's next round consumes the flow queue, reads
// the result and the context it left for itself, and continues.
//
// Flow queues are created on demand (EnsureFlowQueue) — they are not in
// rabbitmq/definitions.json — because every round can spawn its own.
package queue

import (
	"context"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// FlowQueuePrefix marks a queue as a flow (result) queue.
const FlowQueuePrefix = "flow."

// FlowQueueName returns the queue name for a flow.
func FlowQueueName(name string) string { return FlowQueuePrefix + name }

// IsFlowQueue reports whether name is a flow (result) queue.
func IsFlowQueue(name string) bool { return strings.HasPrefix(name, FlowQueuePrefix) }

// EnsureFlowQueue declares the flow queue (durable, with the dead-letter
// args — see dlx.go) and binds it to the tasks exchange on <name>.#, so a
// message published before any consumer exists is held on the broker until
// it is consumed or dead-letters with a reason. Re-declaring with
// identical args is a no-op, which is what makes this safe to call on
// every publish; a pre-EVB-10 queue (no args) is migrated by
// declareWithDLX when it holds nothing.
func (c *Conn) EnsureFlowQueue(name string) error {
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("flow %s: %w", name, err)
	}
	if err := c.declareWithDLX(name); err != nil {
		return fmt.Errorf("flow %s: declare: %w", name, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	// Synchronous bind — see EnsureCapQueue: a noWait bind returns
	// before the broker has registered the binding, and a publish on
	// another channel of the same connection can race ahead of it.
	if err := ch.QueueBind(name, name+".#", c.Exchange(), false, nil); err != nil {
		return fmt.Errorf("flow %s: bind: %w", name, err)
	}
	return nil
}

// PublishFlow publishes m to the flow queue, creating and binding it
// first, and blocks until the broker confirms the publish. A flow queue
// is not a priority queue: ordering is guaranteed by the single consumer
// (the brain's next round), so the message's priority is not sent.
func (c *Conn) PublishFlow(ctx context.Context, name string, m Message) error {
	if err := c.EnsureFlowQueue(name); err != nil {
		return err
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("flow %s: confirm mode: %w", name, err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub, err := newPublishing(m, 0)
	if err != nil {
		return fmt.Errorf("flow %s: encode job %s: %w", name, m.ID, err)
	}
	if err := ch.PublishWithContext(ctx, c.Exchange(), name+".default", false, false, pub); err != nil {
		return fmt.Errorf("flow %s: publish: %w", name, err)
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return fmt.Errorf("flow %s: broker nacked the message", name)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("flow %s: timed out waiting for confirm", name)
	case <-ctx.Done():
		return fmt.Errorf("flow %s: waiting for confirm: %w", name, ctx.Err())
	}
	return nil
}

// PopFlow removes up to n messages from the flow queue (acking them) and
// returns them in order. An empty queue returns no messages and no error.
// The queue must exist (PublishFlow/EnsureFlowQueue created it).
func (c *Conn) PopFlow(name string, n int) ([]Message, error) {
	if n < 1 {
		n = 1
	}
	ch, err := c.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	var out []Message
	for len(out) < n {
		del, ok, err := ch.Get(name, true)
		if err != nil {
			return nil, fmt.Errorf("flow %s: get: %w", name, err)
		}
		if !ok {
			break // queue empty
		}
		m, err := DecodeMessage(del.Body)
		if err != nil {
			return nil, fmt.Errorf("flow %s: decode: %w", name, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// PeekFlow returns up to n messages from the flow queue without removing
// them. It is how a round inspects what a worker left before committing
// to consuming it. Implementation: fetch up to n unacked, then nack them
// with requeue in REVERSE order — a requeued message returns to the front
// of the queue, so reversing the puts-back restores the original order.
// (Requeueing one at a time as it is read would re-fetch the same front
// message every time.) If the process dies between fetch and put-back the
// broker requeues the unacked messages on channel close, so nothing is
// lost.
func (c *Conn) PeekFlow(name string, n int) ([]Message, error) {
	if n < 1 {
		n = 1
	}
	ch, err := c.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	var dels []amqp.Delivery
	for len(dels) < n {
		del, ok, err := ch.Get(name, false)
		if err != nil {
			return nil, fmt.Errorf("flow %s: get: %w", name, err)
		}
		if !ok {
			break // queue empty
		}
		dels = append(dels, del)
	}
	var out []Message
	for i := range dels {
		m, err := DecodeMessage(dels[i].Body)
		if err != nil {
			// Requeue the already-read ones (reverse order keeps their
			// relative order at the front); drop the undecodable one.
			for j := i - 1; j >= 0; j-- {
				dels[j].Nack(false, true)
			}
			dels[i].Nack(false, false)
			return nil, fmt.Errorf("flow %s: decode: %w", name, err)
		}
		out = append(out, m)
	}
	for i := len(dels) - 1; i >= 0; i-- {
		if err := dels[i].Nack(false, true); err != nil {
			return nil, fmt.Errorf("flow %s: requeue: %w", name, err)
		}
	}
	return out, nil
}

// PurgeFlow removes all messages from the flow queue (the queue itself
// stays) and returns how many were removed. Used to start a flow over
// after a failed run.
func (c *Conn) PurgeFlow(name string) (int, error) {
	ch, err := c.NewChannel()
	if err != nil {
		return 0, fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	n, err := ch.QueuePurge(name, false)
	if err != nil {
		return 0, fmt.Errorf("flow %s: purge: %w", name, err)
	}
	return n, nil
}

// DeleteFlow deletes the flow queue and its unacked messages. An empty
// flow queue left by a finished flow is harmless (durable, unbound to any
// consumer), but deleting it keeps the broker tidy.
func (c *Conn) DeleteFlow(name string) error {
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("flow %s: %w", name, err)
	}
	defer ch.Close()
	if _, err := ch.QueueDelete(name, true, false, false); err != nil {
		return fmt.Errorf("flow %s: delete: %w", name, err)
	}
	return nil
}
