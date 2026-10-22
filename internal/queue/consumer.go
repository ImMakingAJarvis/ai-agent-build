// Package queue provides an AMQP consumer that reads from the voice, fix, coding, and background queues.
package queue

import (
	"fmt"
	"log"
	"sync/atomic"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Handler is called with each delivered job message.
// Return an error to nack the message; the worker decides whether to requeue.
// Return ErrQueuePaused to leave the message unacknowledged (it stays pending
// in RabbitMQ until the queue is unpaused and the consumer reprocesses it).
type Handler func(Message) error

// ErrQueuePaused is returned by the handler when a queue is paused by the
// priority scheduler. The consumer leaves the message unacked so it remains
// pending rather than being discarded.
var ErrQueuePaused = fmt.Errorf("queue paused")

// ErrApprovalDenied is returned by the handler when a job's estimated cost
// exceeds the approval threshold. The consumer leaves the message unacked so
// it stays pending until manually approved via the dashboard or Discord.
var ErrApprovalDenied = fmt.Errorf("approval denied: estimated cost exceeds threshold")

// Consumer manages concurrent consumers on the managed queues with manual ack and QoS.
type Consumer struct {
	conn    *Conn
	handler Handler
	// running tracks whether the consumer loop is active.
	running atomic.Bool
	// deliveries holds the channels from Queue.Consume per queue.
	deliveries map[string]<-chan amqp.Delivery
}

// NewConsumer creates a consumer that will read from all managed queues.
func NewConsumer(conn *Conn, handler Handler) *Consumer {
	return &Consumer{conn: conn, handler: handler, deliveries: map[string]<-chan amqp.Delivery{}}
}

// Start declares queues, sets QoS prefetch=1, and begins consuming on each queue.
func (c *Consumer) Start() error {
	ch := c.conn.Channel()

	// Declare queues (idempotent).
	for _, q := range AllQueues[:] {
		if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare queue %s: %w", q, err)
		}
		// Bind to the tasks exchange with the appropriate routing key.
		routingKey := q + ".#"
		if err := ch.QueueBind(q, routingKey, c.conn.Exchange(), true, nil); err != nil {
			return fmt.Errorf("bind %s to %s: %w", q, c.conn.Exchange(), err)
		}
	}

	// Consume each queue with prefetch=1.
	for _, q := range AllQueues {
		if err := ch.Qos(1, 0, false); err != nil {
			return fmt.Errorf("qos on %s: %w", q, err)
		}
		dels, err := ch.Consume(q, "", false, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("consume %s: %w", q, err)
		}
		c.deliveries[q] = dels
	}

	c.running.Store(true)

	// Start a goroutine per queue.
	for _, q := range AllQueues {
		go c.consumeLoop(q, c.deliveries[q])
	}

	log.Printf("consumer: listening on %s, %s, %s, %s", AllQueues[0], AllQueues[1], AllQueues[2], AllQueues[3])
	return nil
}

// Stop signals the consumer to shut down.
func (c *Consumer) Stop() {
	c.running.Store(false)
}

func (c *Consumer) consumeLoop(queueName string, dels <-chan amqp.Delivery) {
	log.Printf("consumer: goroutine started for %s", queueName)
	for {
		if !c.running.Load() {
			log.Printf("consumer: goroutine stopping for %s", queueName)
			return
		}
		del, ok := <-dels
		if !ok {
			log.Printf("consumer: deliveries channel closed for %s", queueName)
			return
		}
		if err := c.handleOne(queueName, del); err != nil {
			log.Printf("consumer: %s: %v", queueName, err)
		}
	}
}

func (c *Consumer) handleOne(queueName string, del amqp.Delivery) error {
	msg, err := DecodeMessage(del.Body)
	if err != nil {
		del.Nack(false, false)
		return fmt.Errorf("decode from %s: %w", queueName, err)
	}

	log.Printf("consumer: %s → job %s (type=%s, source=%s)", queueName, msg.ID, msg.Type, msg.Source)

	if err := c.handler(msg); err != nil {
		if err == ErrQueuePaused {
			// Leave unacked — message stays pending in RabbitMQ.
			return nil
		}
		if err == ErrApprovalDenied {
			// Leave unacked — message stays pending for manual approval.
			return nil
		}
		del.Nack(false, false) // do not requeue on handler error
		return fmt.Errorf("job %s: %w", msg.ID, err)
	}

	del.Ack(false)
	log.Printf("consumer: job %s acknowledged", msg.ID)
	return nil
}
