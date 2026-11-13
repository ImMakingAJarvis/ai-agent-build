package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// CapabilityClient submits a task to a capability worker and waits for the
// Result — the no-LLM-round execution path for a card that carries a
// capability: (plan §3.7). The real implementation
// (NewQueueCapabilityClient) publishes an envelope-v2 capability message to
// cap.<capability> with ReplyTo flow.<card-id> and CorrelationID = card id,
// then polls that flow queue for the matching Result; tests substitute a
// scripted one so the path is exercisable without a live broker.
type CapabilityClient interface {
	// Run submits capability to cap.<capability> (ReplyTo =
	// flow.<corr>, CorrelationID = corr) carrying input, and waits —
	// bounded by ctx — for the Result with CorrelationID corr on that
	// flow queue. It returns the Result once it lands: Status ok, error,
	// timeout or cancelled. A ctx deadline is returned as a Result with
	// Status timeout (not an error), so the caller can settle the card
	// from it; a submit failure or a ctx cancellation (a shutdown) is a
	// non-nil error.
	Run(ctx context.Context, capability, corr string, input map[string]any) (queue.Result, error)
}

// queueCapabilityClient is the production CapabilityClient: it talks to the
// broker directly (publish on the cap queue, poll the per-card flow queue).
type queueCapabilityClient struct {
	conn *queue.Conn
}

// NewQueueCapabilityClient wraps a broker connection as a CapabilityClient.
func NewQueueCapabilityClient(conn *queue.Conn) CapabilityClient {
	return &queueCapabilityClient{conn: conn}
}

func (c *queueCapabilityClient) Run(ctx context.Context, capability, corr string, input map[string]any) (queue.Result, error) {
	flowQueue := queue.FlowQueueName(corr)
	if !queue.ValidCapabilityName(capability) {
		return queue.Result{}, fmt.Errorf("capability %q is not a valid capability name (lowercase dot-separated, e.g. image.describe)", capability)
	}
	if err := c.ensure(ctx, queue.CapQueueName(capability), flowQueue); err != nil {
		return queue.Result{}, err
	}
	// A re-triggered card reuses the same corr (the card id), so a Result a
	// previous run left on the flow queue (it timed out before consuming)
	// would otherwise be mistaken for this run's. Purge it first: the flow
	// queue is named after the card and only this run consumes it, so the
	// purge is safe. It must precede the publish — purging after would
	// delete a fast capability's fresh Result.
	if _, err := c.conn.PurgeFlow(flowQueue); err != nil {
		return queue.Result{}, fmt.Errorf("purge %s: %w", flowQueue, err)
	}
	m := queue.Message{
		ID:            "taskrunner-" + corr,
		Type:          "capability",
		CreatedAt:     time.Now().UTC(),
		Source:        "task-runner",
		Payload:       map[string]string{},
		SchemaVersion: 2,
		Capability:    capability,
		ReplyTo:       flowQueue,
		CorrelationID: corr,
		Input:         input,
	}
	if err := c.conn.Publish(m, queue.CapQueueName(capability)+".default"); err != nil {
		return queue.Result{}, fmt.Errorf("publish %s: %w", queue.CapQueueName(capability), err)
	}
	return c.await(ctx, flowQueue, corr)
}

// ensure declares the cap queue and the per-card flow queue, reconnecting
// first when the broker connection is down. Both are durable, so a
// capability that is published before any worker exists is held on the
// broker, and the Result is held on the flow queue until it is read.
func (c *queueCapabilityClient) ensure(ctx context.Context, capQ, flowQ string) error {
	if c.conn.Dead() {
		if err := c.conn.Reconnect(); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
	}
	if err := c.conn.EnsureCapQueue(capQ); err != nil {
		return err
	}
	if err := c.conn.EnsureFlowQueue(flowQ); err != nil {
		return err
	}
	return nil
}

// await polls flowQueue for the Result whose CorrelationID is corr until it
// lands or ctx is done. A dead broker is reconnected and retried on the
// next pass (the poll interval is the backoff); a ctx deadline maps to a
// timeout Result, a ctx cancellation (shutdown) to the ctx error.
func (c *queueCapabilityClient) await(ctx context.Context, flowQueue, corr string) (queue.Result, error) {
	const poll = 2 * time.Second
	for {
		if c.conn.Dead() {
			// Best effort: a failed reconnect is retried on the next pass,
			// so a brief broker blip does not abort the wait.
			_ = c.conn.Reconnect()
		} else if res, ok, err := c.popResult(flowQueue, corr); err == nil {
			if ok {
				return res, nil
			}
		}
		select {
		case <-ctx.Done():
			return c.doneOrTimeout(ctx, corr)
		case <-time.After(poll):
		}
	}
}

// popResult makes one non-blocking pass over flowQueue: it returns the
// first Result whose CorrelationID is corr (acked), or (…, false, nil) when
// the queue is empty or holds a message that is not this card's Result. A
// message that is not ours is requeued, so a switchboard or other consumer
// on the same queue still sees it.
func (c *queueCapabilityClient) popResult(flowQueue, corr string) (queue.Result, bool, error) {
	ch, err := c.conn.NewChannel()
	if err != nil {
		return queue.Result{}, false, err
	}
	defer ch.Close()
	del, ok, err := ch.Get(flowQueue, false)
	if err != nil {
		return queue.Result{}, false, err
	}
	if !ok {
		return queue.Result{}, false, nil
	}
	res, derr := queue.DecodeResult(del.Body)
	if derr == nil && res.Status != "" && res.CorrelationID == corr {
		if err := del.Ack(false); err != nil {
			return queue.Result{}, false, err
		}
		return res, true, nil
	}
	if err := del.Nack(false, true); err != nil {
		return queue.Result{}, false, err
	}
	return queue.Result{}, false, nil
}

// doneOrTimeout maps a ctx.Done() to either a timeout Result (the wait
// deadline fired — the caller settles the card as a timeout) or the ctx
// error (the process is shutting down — the run is aborted and retried).
func (c *queueCapabilityClient) doneOrTimeout(ctx context.Context, corr string) (queue.Result, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return queue.Result{
			CorrelationID: corr,
			Status:        queue.ResultTimeout,
			Error:         fmt.Sprintf("no Result on %s before the wait deadline", queue.FlowQueueName(corr)),
		}, nil
	}
	return queue.Result{}, ctx.Err()
}
