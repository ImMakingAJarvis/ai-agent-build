// Package queue provides AMQP connection and channel management for RabbitMQ.
package queue

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	defaultAMQPURL     = "amqp://jarvis:jarvis-local@localhost:5672"
	defaultExchange    = "tasks"
	defaultReconnectMs = 5000
)

// Conn wraps an AMQP connection and channel with automatic reconnect.
type Conn struct {
	url       string
	exchange  string
	mu        sync.Mutex
	conn      *amqp.Connection
	channel   *amqp.Channel
	closed    bool
}

// NewConn creates a connection to RabbitMQ and declares the exchange.
func NewConn() (*Conn, error) {
	url := os.Getenv("RABBITMQ_URL")
	if url == "" {
		url = defaultAMQPURL
	}
	c := &Conn{url: url, exchange: defaultExchange}
	if err := c.connect(); err != nil {
		return nil, fmt.Errorf("queue: connect: %w", err)
	}
	return c, nil
}

func (c *Conn) connect() error {
	var err error
	c.conn, err = amqp.Dial(c.url)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.url, err)
	}
	c.channel, err = c.conn.Channel()
	if err != nil {
		c.conn.Close()
		return fmt.Errorf("open channel: %w", err)
	}
	// Declare the topic exchange (idempotent).
	if err := c.channel.ExchangeDeclare(
		c.exchange, "topic", true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("declare exchange %s: %w", c.exchange, err)
	}
	return nil
}

// Reconnect re-establishes the connection and channel. Safe to call concurrently.
func (c *Conn) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channel.Close()
	c.conn.Close()
	return c.connect()
}

// Channel returns the current AMQP channel.
func (c *Conn) Channel() *amqp.Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channel
}

// Exchange returns the exchange name.
func (c *Conn) Exchange() string { return c.exchange }

// Close shuts down the connection. After Close, Reconnect can revive it.
func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.channel.Close()
	c.conn.Close()
}

// IsClosed reports whether Close has been called.
func (c *Conn) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Publish sends a JSON-encoded Message to the exchange with the given routing key.
func (c *Conn) Publish(m Message, routingKey string) error {
	c.mu.Lock()
	ch := c.channel
	c.mu.Unlock()

	body, err := encodeMessage(m)
	if err != nil {
		return fmt.Errorf("encode job %s: %w", m.ID, err)
	}

	return ch.PublishWithContext(context.Background(), c.exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		Priority:     uint8(m.Priority),
		DeliveryMode: amqp.Persistent,
		Timestamp:    m.CreatedAt,
	})
}

// WaitForReady blocks until the connection is open and the channel is usable.
// Reconnects on failure, backing off up to 30 s.
func (c *Conn) WaitForReady() {
	backoff := time.Second
	for {
		c.mu.Lock()
		if c.conn != nil && c.conn.IsClosed() && !c.closed {
			c.mu.Unlock()
			if err := c.Reconnect(); err == nil {
				log.Printf("queue: reconnected to RabbitMQ")
				break
			}
		} else {
			c.mu.Unlock()
			break
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
