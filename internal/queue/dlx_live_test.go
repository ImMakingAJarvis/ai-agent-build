package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TestFlowAndCapQueuesDeclareWithDLXArgs is a live test (RabbitMQ on
// RABBITMQ_URL, default localhost:5672): every cap.*/flow.* queue must be
// declared with the dead-letter args, so a rejected message has somewhere
// to go. Skips when the broker is unreachable, so `go test ./...` stays
// green off this machine.
func TestFlowAndCapQueuesDeclareWithDLXArgs(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	flowName := FlowQueueName(fmt.Sprintf("dlx-args-%d", time.Now().UnixNano()))
	capName := CapQueueName(fmt.Sprintf("dlx-args-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	if err := c.EnsureCapQueue(capName); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	defer c.deleteBoth(t, flowName, capName)

	for _, name := range []string{flowName, capName} {
		// A redeclare with identical args must be a no-op success...
		ch, err := c.NewChannel()
		if err != nil {
			t.Fatalf("channel: %v", err)
		}
		if _, err := ch.QueueDeclare(name, true, false, false, false, dlxArgs()); err != nil {
			t.Errorf("%s: redeclare with dlxArgs failed: %v", name, err)
		}
		// ...and one with no args must 406 — proof the queue carries the
		// dead-letter args (AMQP 0-9-1 exposes no way to read them back).
		// The 406 closes the channel (AMQP semantics), so each name gets
		// its own.
		var amqpErr *amqp.Error
		if _, err := ch.QueueDeclare(name, true, false, false, false, nil); !errors.As(err, &amqpErr) || amqpErr.Code != amqp.PreconditionFailed {
			t.Errorf("%s: redeclare with nil args = %v, want PRECONDITION_FAILED (406)", name, err)
		}
		ch.Close()
	}
}

// TestPreExistingQueueMigratesToDLX is a live test: a flow queue declared
// before dead-lettering (no args) is migrated by EnsureFlowQueue when it
// is empty, and kept untouched when it holds messages — the migration
// must never delete a queue that still has work on it.
func TestPreExistingQueueMigratesToDLX(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	predeclare := func(name string) {
		t.Helper()
		ch, err := c.NewChannel()
		if err != nil {
			t.Fatalf("predeclare %s: channel: %v", name, err)
		}
		defer ch.Close()
		if _, err := ch.QueueDeclare(name, true, false, false, false, nil); err != nil {
			t.Fatalf("predeclare %s: %v", name, err)
		}
		if err := ch.QueueBind(name, name+".#", c.Exchange(), true, nil); err != nil {
			t.Fatalf("predeclare %s: bind: %v", name, err)
		}
	}

	// 1. Empty pre-EVB-10 queue: migrated, DLX args end up on it.
	empty := FlowQueueName(fmt.Sprintf("dlx-migrate-%d", time.Now().UnixNano()))
	predeclare(empty)
	if err := c.EnsureFlowQueue(empty); err != nil {
		t.Fatalf("ensure (migrate empty): %v", err)
	}
	if !hasDLXArgs(c, empty) {
		t.Errorf("empty queue %s was not migrated: no DLX args", empty)
	}
	c.deleteQueue(t, empty)

	// 2. Non-empty pre-EVB-10 queue: kept as-is, message preserved.
	occupied := FlowQueueName(fmt.Sprintf("dlx-keep-%d", time.Now().UnixNano()))
	predeclare(occupied)
	pub, err := newPublishing(Message{ID: "keep-me", Type: "image"}, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := ch.PublishWithContext(ctx, c.Exchange(), occupied+".default", false, false, pub); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ch.Close()
	if err := c.EnsureFlowQueue(occupied); err != nil {
		t.Fatalf("ensure (keep occupied): %v", err)
	}
	if depth, _ := c.QueueDepth(occupied); depth != 1 {
		t.Errorf("occupied queue depth = %d, want 1 (the message must survive)", depth)
	}
	if hasDLXArgs(c, occupied) {
		t.Errorf("occupied queue %s was migrated anyway — its message would be lost", occupied)
	}
	c.deleteQueue(t, occupied)
}

// hasDLXArgs probes a queue's declared arguments the only way AMQP 0-9-1
// allows: a redeclare with the DLX args succeeds iff they match.
func hasDLXArgs(c *Conn, name string) bool {
	ch, err := c.NewChannel()
	if err != nil {
		return false
	}
	defer ch.Close()
	_, err = ch.QueueDeclare(name, true, false, false, false, dlxArgs())
	return err == nil
}

// TestPoisonMessageLandsInDead is a live test: a message no consumer can
// decode (a poison) must not vanish — the consumer nacks it without
// requeue and it lands on the dead queue with its reason, and the
// requeue/drop actions both work on it.
func TestPoisonMessageLandsInDead(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	flowName := FlowQueueName(fmt.Sprintf("dlx-poison-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer c.deleteQueue(t, flowName)

	rk := flowName + ".default"
	// 1. The poison: publish raw bytes a Message decoder cannot parse.
	if err := publishRaw(c, rk, []byte("poison-1, not a job message")); err != nil {
		t.Fatalf("publish poison: %v", err)
	}
	// 2. The consumer's path (PeekFlow/handleOne both do this): get it,
	//    fail to decode, nack without requeue.
	if err := rejectUndecodable(c, flowName); err != nil {
		t.Fatalf("reject: %v", err)
	}
	dl := waitForDead(t, c, rk)
	if dl.Queue != flowName {
		t.Errorf("dead letter queue = %q, want %q", dl.Queue, flowName)
	}
	if dl.Reason != "rejected" {
		t.Errorf("dead letter reason = %q, want rejected", dl.Reason)
	}
	if dl.Preview == "" {
		t.Errorf("dead letter has no body preview: %+v", dl)
	}

	// 3. Requeue: back to the original queue, dead letter gone.
	if err := c.RequeueDead(dl.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	depth, err := c.QueueDepth(flowName)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != 1 {
		t.Errorf("queue depth after requeue = %d, want 1", depth)
	}
	if got := listDeadByRK(c, rk); got != 0 {
		t.Errorf("%d dead letters with routing key %s after requeue, want 0", got, rk)
	}

	// 4. Drop: dead-letter the poison a second time, then discard it.
	if err := publishRaw(c, rk, []byte("poison-2, not a job message either")); err != nil {
		t.Fatalf("publish poison 2: %v", err)
	}
	if err := rejectUndecodable(c, flowName); err != nil {
		t.Fatalf("reject 2: %v", err)
	}
	dl2 := waitForDead(t, c, rk)
	if err := c.DropDead(dl2.ID); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if got := listDeadByRK(c, rk); got != 0 {
		t.Errorf("%d dead letters with routing key %s after drop, want 0", got, rk)
	}
	depth, _ = c.QueueDepth(flowName)
	if depth != 1 {
		t.Errorf("queue depth after drop = %d, want 1 (the requeued poison stays)", depth)
	}
}

// TestCapPoisonMessageLandsInDead is a live test: the same guarantee on a
// capability queue — the other queue family in scope (EVB-10).
func TestCapPoisonMessageLandsInDead(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	capName := CapQueueName(fmt.Sprintf("dlx-cap-%d", time.Now().UnixNano()))
	if err := c.EnsureCapQueue(capName); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer c.deleteQueue(t, capName)

	rk := capName + ".default"
	if err := publishRaw(c, rk, []byte("cap poison")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := rejectUndecodable(c, capName); err != nil {
		t.Fatalf("reject: %v", err)
	}
	dl := waitForDead(t, c, rk)
	if dl.Queue != capName || dl.Reason != "rejected" {
		t.Errorf("dead letter = %+v, want queue %s reason rejected", dl, capName)
	}
	if err := c.DropDead(dl.ID); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

// TestRequeueDeadRecreatesMissingQueue is a live test: a dead letter whose
// original queue was deleted must be requeued by recreating that queue —
// the republish lands on the queue named in x-death, not on a queue with
// the prefix stripped off.
func TestRequeueDeadRecreatesMissingQueue(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	flowName := FlowQueueName(fmt.Sprintf("dlx-recreate-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	rk := flowName + ".default"
	if err := publishRaw(c, rk, []byte("recreate victim")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := rejectUndecodable(c, flowName); err != nil {
		t.Fatalf("reject: %v", err)
	}
	dl := waitForDead(t, c, rk)
	c.deleteQueue(t, flowName) // the queue is gone while the message is dead
	if err := c.RequeueDead(dl.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	depth, err := c.QueueDepth(flowName)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != 1 {
		t.Errorf("recreated queue %s depth = %d, want 1 (the republish must land on the original queue name)", flowName, depth)
	}
	if d, e := c.QueueDepth(strings.TrimPrefix(flowName, FlowQueuePrefix)); e == nil && d != 0 {
		t.Errorf("prefix-stripped queue holds %d messages — the republish went to the wrong queue", d)
	}
	c.deleteQueue(t, flowName)
}

// TestAttemptsExhaustedLandsInDead is a live test: the delivery-budget
// convention — a consumer that fails every attempt republishes with
// BumpAttempt until the count reaches MaxAttempts, then nacks without
// requeue, and the broker dead-letters the message (reason "rejected").
func TestAttemptsExhaustedLandsInDead(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	flowName := FlowQueueName(fmt.Sprintf("dlx-attempts-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer c.deleteQueue(t, flowName)

	ctx := context.Background()
	rk := flowName + ".default"
	pub, err := newPublishing(Message{ID: "retry-victim", Type: "image"}, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	if err := ch.PublishWithContext(ctx, c.Exchange(), rk, false, false, pub); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deliveries := 0
	for {
		del, ok, err := ch.Get(flowName, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !ok {
			t.Fatalf("queue empty after %d deliveries, budget path broken", deliveries)
		}
		deliveries++
		if AttemptCount(del) != deliveries-1 {
			t.Fatalf("delivery %d: x-attempts = %d, want %d", deliveries, AttemptCount(del), deliveries-1)
		}
		if AttemptCount(del) >= MaxAttempts-1 {
			// Budget exhausted: give up — the broker dead-letters it.
			if err := del.Nack(false, false); err != nil {
				t.Fatalf("final nack: %v", err)
			}
			break
		}
		// Transient failure: republish with the bumped count (a
		// nack-requeue cannot carry new headers).
		if err := del.Ack(false); err != nil {
			t.Fatalf("ack: %v", err)
		}
		retry := amqp.Publishing{
			Headers:      BumpAttempt(del),
			Body:         del.Body,
			DeliveryMode: amqp.Persistent,
		}
		if err := ch.PublishWithContext(ctx, c.Exchange(), rk, false, false, retry); err != nil {
			t.Fatalf("republish: %v", err)
		}
	}
	if deliveries != MaxAttempts {
		t.Errorf("%d deliveries, want %d (the budget)", deliveries, MaxAttempts)
	}
	dl := waitForDead(t, c, rk)
	if dl.Reason != "rejected" {
		t.Errorf("dead letter reason = %q, want rejected", dl.Reason)
	}
	if dl.Count != 1 {
		t.Errorf("dead letter count = %d, want 1 (first death)", dl.Count)
	}
	if err := c.DropDead(dl.ID); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

// TestExpiredMessageLandsInDead is a live test: a message that outlives
// its per-message TTL is dead-lettered by the broker itself (reason
// "expired") — no consumer involved at all.
func TestExpiredMessageLandsInDead(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	flowName := FlowQueueName(fmt.Sprintf("dlx-ttl-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer c.deleteQueue(t, flowName)

	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	pub, err := newPublishing(Message{ID: "ttl-victim", Type: "image"}, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pub.Expiration = "300" // per-message TTL: 300 ms
	if err := ch.PublishWithContext(context.Background(), c.Exchange(), flowName+".default", false, false, pub); err != nil {
		t.Fatalf("publish: %v", err)
	}
	dl := waitForDead(t, c, flowName+".default")
	if dl.Reason != "expired" {
		t.Errorf("dead letter reason = %q, want expired", dl.Reason)
	}
	if err := c.DropDead(dl.ID); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

// --- helpers ---

func (c *Conn) deleteQueue(t *testing.T, name string) {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		return
	}
	defer ch.Close()
	ch.QueueDelete(name, false, false, false)
}

func (c *Conn) deleteBoth(t *testing.T, names ...string) {
	for _, n := range names {
		c.deleteQueue(t, n)
	}
}

// publishRaw publishes raw bytes to the tasks exchange with rk (a poison
// is by definition not an encoded Message).
func publishRaw(c *Conn, rk string, body []byte) error {
	ch, err := c.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	return ch.PublishWithContext(context.Background(), c.Exchange(), rk, false, false, amqp.Publishing{
		Body:         body,
		DeliveryMode: amqp.Persistent,
	})
}

// rejectUndecodable plays the consumer: get the head of the queue, fail
// to decode it, nack without requeue — the code path that must now
// dead-letter instead of dropping the message.
func rejectUndecodable(c *Conn, name string) error {
	ch, err := c.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	del, ok, err := ch.Get(name, false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("queue %s is empty, poison not there", name)
	}
	if _, err := DecodeMessage(del.Body); err == nil {
		del.Ack(false)
		return fmt.Errorf("body %q decoded as a Message, not a poison", del.Body)
	}
	return del.Nack(false, false)
}

// waitForDead polls the dead queue until a dead letter with routing key rk
// shows up (dead-lettering is asynchronous for the expiry path).
func waitForDead(t *testing.T, c *Conn, rk string) DeadLetter {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		items, err := c.ListDead(50)
		if err != nil {
			t.Fatalf("list dead: %v", err)
		}
		for _, dl := range items {
			if dl.RoutingKey == rk {
				return dl
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no dead letter with routing key %s after 10s", rk)
	return DeadLetter{}
}

func listDeadByRK(c *Conn, rk string) int {
	items, err := c.ListDead(50)
	if err != nil {
		return -1
	}
	n := 0
	for _, dl := range items {
		if dl.RoutingKey == rk {
			n++
		}
	}
	return n
}
