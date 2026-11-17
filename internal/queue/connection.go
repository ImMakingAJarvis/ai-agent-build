// Package queue provides AMQP connection and channel management for RabbitMQ.
package queue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	url      string
	exchange string
	opts     DialOpts
	mu       sync.Mutex
	conn     *amqp.Connection
	channel  *amqp.Channel
	closed   bool
}

// DialOpts tunes one connection beyond the URL (the remote worker's
// off-LAN path, plan §3.9): an explicit broker URL and the TLS trust
// for amqps://. A zero value is the pre-existing behavior: RABBITMQ_URL
// or the default, plain amqp://.
type DialOpts struct {
	// URL is the broker URL (amqp:// or amqps://). Empty = AMQPURL().
	URL string
	// CACert is a PEM CA bundle for verifying the broker's amqps://
	// certificate (the self-signed CA both nodes share). Empty = the
	// system root pool.
	CACert []byte
	// InsecureSkipVerify disables certificate verification (TLS still
	// encrypts the link). LAN/lab only — never for an off-LAN link.
	InsecureSkipVerify bool
}

// AMQPURL is the effective broker URL: RABBITMQ_URL or the default.
// Exported so a caller can derive companion endpoints (the management
// API on the same host, port 15672) from the same source NewConn uses.
func AMQPURL() string {
	if u := os.Getenv("RABBITMQ_URL"); u != "" {
		return u
	}
	return defaultAMQPURL
}

// NewConn creates a connection to RabbitMQ and declares the exchange.
func NewConn() (*Conn, error) {
	return NewConnOpts(DialOpts{})
}

// NewConnOpts creates a connection with explicit dial options (the
// remote worker's config-file path, plan §3.9): the broker URL and, for
// an off-LAN amqps:// link, the CA bundle or the insecure flag. A zero
// DialOpts is NewConn.
func NewConnOpts(o DialOpts) (*Conn, error) {
	url := o.URL
	if url == "" {
		url = AMQPURL()
	}
	c := &Conn{url: url, exchange: defaultExchange, opts: o}
	if err := c.connect(); err != nil {
		return nil, fmt.Errorf("queue: connect: %w", err)
	}
	return c, nil
}

// tlsConfig builds the TLS config for a tuned connection, reporting
// tuned=false when there is nothing to tune (no CA, not insecure) so
// the plain amqp:// path stays exactly amqp.Dial.
func (o DialOpts) tlsConfig() (cfg *tls.Config, tuned bool, err error) {
	if len(o.CACert) == 0 && !o.InsecureSkipVerify {
		return nil, false, nil
	}
	cfg = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.InsecureSkipVerify}
	if len(o.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.CACert) {
			return nil, false, fmt.Errorf("no certificates parsed from the CA bundle")
		}
		cfg.RootCAs = pool
	}
	return cfg, true, nil
}

func (c *Conn) connect() error {
	var err error
	c.conn, err = c.dial()
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

// dial opens the AMQP connection: plain amqp.Dial unless DialOpts tunes
// the TLS trust (an off-LAN amqps:// link with a shared CA, plan §3.9).
func (c *Conn) dial() (*amqp.Connection, error) {
	tlsCfg, tuned, err := c.opts.tlsConfig()
	if err != nil {
		return nil, err
	}
	if !tuned {
		return amqp.Dial(c.url)
	}
	return amqp.DialConfig(c.url, amqp.Config{TLSClientConfig: tlsCfg})
}

// Reconnect re-establishes the connection and channel if the underlying
// AMQP connection is dead (broker restart, network drop). A live
// connection is left in place: closing it would drop the channels other
// callers hold (the location workers, the publishers), and workers
// resubscribing after the same drop would each close the connection the
// previous one just opened — flapping forever with no queue holding a
// consumer (2026-11-08 09:20). Safe to call concurrently.
func (c *Conn) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && !c.conn.IsClosed() {
		return nil
	}
	// A failed Dial leaves conn nil (connect assigns the error value), so the
	// close of the previous attempt's leftovers must tolerate nil — the
	// switchboard's poll loop calls Reconnect every 250 ms while the broker is
	// down and the second call used to SIGSEGV on the nil conn (2026-11-09,
	// Phase 2 gate S3).
	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil {
		c.conn.Close()
	}
	return c.connect()
}

// Channel returns the current AMQP channel.
func (c *Conn) Channel() *amqp.Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channel
}

// NewChannel opens a dedicated AMQP channel on this connection. The
// location workers (A6) each use one so their prefetch=1 is independent
// of the main consumer's shared channel: a job running on one queue can
// never hold another queue's delivery slot.
func (c *Conn) NewChannel() (*amqp.Channel, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		return nil, fmt.Errorf("not connected")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	return ch, nil
}

// QueueDepth inspects one queue on a throwaway channel so a missing
// queue (AMQP 404) cannot close the consumer's channel.
func (c *Conn) QueueDepth(name string) (int, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0, fmt.Errorf("not connected")
	}
	ch, err := conn.Channel()
	if err != nil {
		return 0, err
	}
	defer ch.Close()
	q, err := ch.QueueInspect(name)
	if err != nil {
		return 0, err
	}
	return q.Messages, nil
}

// Exchange returns the exchange name.
func (c *Conn) Exchange() string { return c.exchange }

// Close shuts down the connection. After Close, Reconnect can revive it.
// Tolerates a nil conn/channel (a Dial that never succeeded).
func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}

// IsClosed reports whether Close has been called.
func (c *Conn) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Dead reports whether the underlying AMQP connection is gone (broker
// restart, network drop) — as opposed to IsClosed, which only reports a
// Close we initiated. The publisher watchdog uses this to Reconnect.
func (c *Conn) Dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn == nil || c.conn.IsClosed()
}

// Publish sends a JSON-encoded Message to the exchange with the given routing key.
func (c *Conn) Publish(m Message, routingKey string) error {
	c.mu.Lock()
	ch := c.channel
	c.mu.Unlock()
	return c.PublishOn(ch, m, routingKey)
}

// PublishOn is Publish on a specific channel. jarvis-mq call needs it for
// the direct reply-to trick (plan §3.8): the broker rewrites a request's
// reply-to to the caller's private queue only when the PUBLISHING channel
// is the one that consumes amq.rabbitmq.reply-to — the rewrite state is
// per-channel, not per-connection. A publish on any other channel of the
// same connection is rejected with "fast reply consumer does not exist",
// and because the channel exception arrives asynchronously,
// PublishWithContext returns nil: the request never leaves the client and
// the caller just times out (verified against this broker's logs).
func (c *Conn) PublishOn(ch *amqp.Channel, m Message, routingKey string) error {
	pub, err := newPublishing(m, uint8(m.Priority))
	if err != nil {
		return fmt.Errorf("encode job %s: %w", m.ID, err)
	}
	return ch.PublishWithContext(context.Background(), c.exchange, routingKey, false, false, pub)
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
