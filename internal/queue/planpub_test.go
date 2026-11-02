package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TestPlanPublisherFrontOfQueue is the B1 acceptance check: a normal
// background job is published first, then a plan_epic job, and the
// consumer sees the plan job first. The scratch queue is declared with
// x-max-priority 10 — the same definition the background queue carries after
// the B1 migration (see docs/work-board/NOTES.md) — because a queue
// without x-max-priority ignores the wire priority entirely.
func TestPlanPublisherFrontOfQueue(t *testing.T) {
	conn := liveConn(t)
	name := fmt.Sprintf("b1test.background.%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteQueue(t, conn, name) })

	ch, err := conn.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	args := amqp.Table{"x-max-priority": int64(PlanEpicPriority)}
	if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
		t.Fatalf("declare scratch queue: %v", err)
	}
	// noWait=false: the next publish goes out on a different channel
	// (Conn.Publish uses the shared one), and RabbitMQ gives no ordering
	// across channels — an async bind can lose the race and the exchange
	// silently drops the unrouted message.
	if err := ch.QueueBind(name, name+".#", conn.Exchange(), false, nil); err != nil {
		t.Fatalf("bind scratch queue: %v", err)
	}

	// 1. a normal background job (the enqueue CLI's priority for the queue)
	normal := Message{
		ID: "b1-normal", Type: BackgroundQueue, Priority: 1,
		Kind: KindAgent, Title: "normal background job", CreatedAt: time.Now().UTC(),
	}
	if err := conn.Publish(normal, name+".default"); err != nil {
		t.Fatalf("publish normal job: %v", err)
	}
	// AMQP publishes are async: wait for the job to be enqueued so the
	// depth the PlanPublisher reports below is deterministic.
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, err := conn.QueueDepth(name)
		if err != nil {
			t.Fatalf("queue depth: %v", err)
		}
		if d >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the normal job to be enqueued")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 2. the plan_epic job, through the PlanPublisher
	plan := Message{
		ID: "b1-plan", Type: BackgroundQueue, Priority: PlanEpicPriority,
		Kind: KindAgent, Title: "plan epic", CreatedAt: time.Now().UTC(),
	}
	behind, err := NewPlanPublisher(conn, name).PublishPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("PublishPlan: %v", err)
	}
	if behind != 1 {
		t.Errorf("jobs behind the plan job = %d, want 1", behind)
	}

	// 3. consume: the plan job must be delivered first
	dels, err := ch.Consume(name, "", true, false, false, false, nil) // autoAck
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	for i, want := range []string{"b1-plan", "b1-normal"} {
		select {
		case d := <-dels:
			var m Message
			if err := json.Unmarshal(d.Body, &m); err != nil {
				t.Fatalf("delivery %d does not decode: %v", i+1, err)
			}
			if m.ID != want {
				t.Fatalf("delivery %d = %s, want %s (plan job must come first)", i+1, m.ID, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for delivery %d", i+1)
		}
	}
}
