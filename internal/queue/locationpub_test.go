package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestLocationPublisherDelivers: a run_task job published through the
// LocationPublisher reaches the queue (declared plain, the same way
// LocationWorker.Start declares it) and decodes back to the same job.
func TestLocationPublisherDelivers(t *testing.T) {
	conn := liveConn(t)
	name := fmt.Sprintf("c1test.jobs.%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteQueue(t, conn, name) })

	m := Message{
		ID: "plane-run-task-test", Type: "run_task", Kind: KindAgent,
		Title: "Plane: run task TST-3 (toy)", Instructions: `{"trigger":"run_task"}`,
		CreatedAt: time.Now().UTC(),
	}
	if err := NewLocationPublisher(conn).publish(context.Background(), name, m); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ch, err := conn.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	dels, err := ch.Consume(name, "", true, false, false, false, nil) // autoAck
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	select {
	case d := <-dels:
		var got Message
		if err := json.Unmarshal(d.Body, &got); err != nil {
			t.Fatalf("delivery does not decode: %v", err)
		}
		if got.ID != m.ID || got.Type != m.Type || got.Title != m.Title || got.Instructions != m.Instructions {
			t.Errorf("decoded = %+v, want %+v", got, m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the delivery")
	}
}

// TestLocationPublisherRejectsCloud: "cloud" has no box queue — the
// bridge keeps such jobs store-only, and the publisher says so.
func TestLocationPublisherRejectsCloud(t *testing.T) {
	pub := &LocationPublisher{}
	if err := pub.PublishLocation(context.Background(), "cloud", Message{ID: "x"}); err == nil {
		t.Error("PublishLocation(cloud) = nil error, want an error")
	}
}
