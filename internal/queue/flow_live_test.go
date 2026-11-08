package queue

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPeekFlowDoesNotDestroyMessages is a live test (RabbitMQ on
// RABBITMQ_URL, default localhost:5672): peeking a flow queue must return
// the messages without removing them — "peek no longer destroys jobs" (the
// pre-fix jarvis-mq peek consumed with noAck, which removed the messages
// it displayed). It also pins the reverse-order requeue contract: a second
// peek sees the same messages in the same order, and a real pop afterwards
// still gets them all. Skips when the broker is unreachable, so
// `go test ./...` stays green off this machine.
func TestPeekFlowDoesNotDestroyMessages(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	name := FlowQueueName(fmt.Sprintf("peek-test-%d", time.Now().UnixNano()))
	if err := c.EnsureFlowQueue(name); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer func() {
		ch, err := c.NewChannel()
		if err != nil {
			return
		}
		ch.Close()
		ch.QueueDelete(name, false, false, false)
	}()

	ctx := context.Background()
	ids := []string{"peek-a", "peek-b", "peek-c"}
	for _, id := range ids {
		m := Message{ID: id, Type: "image", Payload: map[string]string{"id": id}}
		if err := c.PublishFlow(ctx, name, m); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	got, err := c.PeekFlow(name, 10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("peek returned %d messages, want %d", len(got), len(ids))
	}
	for i, m := range got {
		if m.ID != ids[i] {
			t.Errorf("peek[%d].ID = %q, want %q (order must be preserved)", i, m.ID, ids[i])
		}
	}

	// The point of the test: a second peek sees the same messages — the
	// first one did not destroy them.
	got, err = c.PeekFlow(name, 10)
	if err != nil {
		t.Fatalf("second peek: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("second peek returned %d messages, want %d (peek destroyed the jobs)", len(got), len(ids))
	}

	// Sanity: the nacks left the queue intact — a real pop removes all of
	// them, in order.
	popped, err := c.PopFlow(name, 10)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if len(popped) != len(ids) {
		t.Fatalf("pop returned %d messages, want %d", len(popped), len(ids))
	}
	for i, m := range popped {
		if m.ID != ids[i] {
			t.Errorf("pop[%d].ID = %q, want %q", i, m.ID, ids[i])
		}
	}
}
