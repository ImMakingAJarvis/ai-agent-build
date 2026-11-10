package queue

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TestConsumerPriorityRanking pins the broker's consumer-priority
// semantics that the capworker SDK's placement story relies on (plan
// §3.5): one consumer at x-priority 1 must still receive a message whose
// own priority is 5. If this ever fails, the broker has started gating
// delivery on consumer x-priority >= message priority, and the
// "best free worker" comments in internal/capworker are wrong.
func TestConsumerPriorityRanking(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	q := "q.consumer-priority-ranking"
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare(q, false, true, true, false, amqp.Table{"x-max-priority": 10}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := ch.Qos(1, 0, true); err != nil {
		t.Fatalf("qos: %v", err)
	}
	dels, err := ch.Consume(q, "", false, false, false, false, amqp.Table{"x-priority": 1})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := ch.Publish("", q, false, false, amqp.Publishing{Body: []byte("probe"), Priority: 5, DeliveryMode: amqp.Persistent}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-dels:
		// Consumer priority is a pure ranking of consumers.
	case <-time.After(2 * time.Second):
		t.Fatal("message at priority 5 was not delivered to the free x-priority-1 consumer — the broker now gates on consumer x-priority")
	}
}
