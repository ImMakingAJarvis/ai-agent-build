// Package reelq is Reel's RabbitMQ side: scan jobs on the "reel.scan"
// queue and the gaming-watchdog pause contract (EVB-29).
//
// The consumer mirrors the stack's internal/queue.LocationWorker contract:
// one queue, prefetch 1, and a paused flag. While paused, arriving
// deliveries are held unacked (prefetch 1 means the held delivery also
// blocks the next one, so a paused scan never starts); a scan that is
// already running finishes first because the pause is checked only before
// a handler starts. Resume lets the held delivery through. A dead broker
// is recovered by resubscribing, like the location worker.
//
// The pause itself is driven by the resourced stream controller, which
// publishes stream.started / stream.stopped on the "events" exchange with
// game: true for a Steam game (a Sunshine desktop stream sets game: false
// and coexists with everything). WatchStreams subscribes to stream.# and
// pauses on a game start, resumes on stop; StreamActive polls resourced's
// GET /state so a reeld that starts up mid-game begins paused.
package reelq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker topology. The "tasks" and "events" exchanges are the stack's
// (internal/queue); "reel.scan" is Reel's own queue bound on "tasks", so
// no other stack consumer sees scan jobs and scan jobs never reach the
// location queues.
const (
	TaskExchange  = "tasks"
	EventExchange = "events"
	ScanQueue     = "reel.scan"
	ScanRouting   = "reel.scan"
	ScanType      = "reel.scan" // the message's "type" field
)

// Message is the stack's job envelope (the v1 fields reel uses; extra
// fields encode/decode to zero on both sides).
type Message struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Payload map[string]string `json:"payload"`
}

// ScanJob is a scan message's payload.
type ScanJob struct {
	LibraryID int64
	ScanID    int64
}

// DecodeScanJob extracts the scan job from a message's payload.
func DecodeScanJob(m Message) (ScanJob, error) {
	var j ScanJob
	var err error
	if j.LibraryID, err = strconv.ParseInt(m.Payload["library_id"], 10, 64); err != nil {
		return j, errors.New("scan job missing library_id")
	}
	if j.ScanID, err = strconv.ParseInt(m.Payload["scan_id"], 10, 64); err != nil {
		return j, errors.New("scan job missing scan_id")
	}
	return j, nil
}

// Publisher publishes scan jobs to the tasks exchange.
type Publisher struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewPublisher dials the broker and declares the tasks exchange.
func NewPublisher(amqpURL string) (*Publisher, error) {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return nil, fmt.Errorf("reelq: dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := ch.ExchangeDeclare(TaskExchange, "topic", true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Publisher{conn: conn, ch: ch}, nil
}

// PublishScan enqueues one scan job (persistent, so a broker restart
// redelivers it).
func (p *Publisher) PublishScan(ctx context.Context, libraryID, scanID int64) error {
	m := Message{
		ID:   fmt.Sprintf("scan-%d-%d", scanID, time.Now().UnixNano()),
		Type: ScanType,
		Payload: map[string]string{
			"library_id": strconv.FormatInt(libraryID, 10),
			"scan_id":    strconv.FormatInt(scanID, 10),
		},
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return p.ch.PublishWithContext(ctx, TaskExchange, ScanRouting, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
	})
}

func (p *Publisher) Close() error { return p.conn.Close() }

// EnsureScanQueue declares and binds the scan queue (idempotent). A
// publish that reaches the exchange before the binding exists is dropped
// silently, so reeld — and the queue E2E — call this before enqueuing is
// possible. The consumer re-declares and re-binds on every (re)start as
// well; this only closes the startup race.
func EnsureScanQueue(amqpURL string) error {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(ScanQueue, true, false, false, false, nil); err != nil {
		return err
	}
	return ch.QueueBind(ScanQueue, ScanRouting, TaskExchange, false, nil)
}

// Handler runs one dequeued scan job.
type Handler func(ctx context.Context, j ScanJob) error

// Consumer consumes the scan queue with the LocationWorker pause contract
// (see the package comment).
type Consumer struct {
	url           string
	handler       Handler
	paused        atomic.Bool
	eventExchange string
}

// NewConsumer builds a consumer for amqpURL. It is not connected until
// Start.
func NewConsumer(amqpURL string, h Handler) *Consumer {
	return &Consumer{url: amqpURL, handler: h, eventExchange: EventExchange}
}

// SetEventExchange overrides the exchange stream events are read from.
// Production uses the stack's "events" exchange; the queue E2E test uses a
// private one so its fake game stream cannot pause a live stack worker.
func (c *Consumer) SetEventExchange(name string) { c.eventExchange = name }

// Pause holds arriving deliveries (the gaming watchdog fired).
func (c *Consumer) Pause() { c.paused.Store(true) }

// Resume releases the held delivery, if any.
func (c *Consumer) Resume() { c.paused.Store(false) }

// IsPaused reports the hold state.
func (c *Consumer) IsPaused() bool { return c.paused.Load() }

// Start consumes until ctx is canceled. A broker restart is recovered by
// resubscribing after a short wait; the queue is durable, so held and
// pending jobs survive it.
func (c *Consumer) Start(ctx context.Context) error {
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("reelq: scan queue connection lost, resubscribing", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Consumer) runOnce(ctx context.Context) error {
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(ScanQueue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(ScanQueue, ScanRouting, TaskExchange, false, nil); err != nil {
		return err
	}
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(ScanQueue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for d := range deliveries {
		if ctx.Err() != nil {
			// Leaving a delivery unacked on shutdown is safe: it is
			// persistent and redelivered to the next consumer.
			return nil
		}
		// While paused the delivery stays unacked (see Pause): hold this
		// delivery until Resume so a scan never starts during a game. The
		// prefetch-1 window keeps the rest of the queue behind it.
		for c.paused.Load() {
			if ctx.Err() != nil {
				// Leaving a delivery unacked on shutdown is safe: it is
				// persistent and redelivered to the next consumer.
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := c.handle(d); err != nil {
			slog.Error("reelq: scan job failed", "err", err)
			_ = d.Nack(false, false) // no requeue: the scan row records the error
			continue
		}
		_ = d.Ack(false)
	}
	return errors.New("scan queue channel closed")
}

func (c *Consumer) handle(d amqp.Delivery) error {
	var m Message
	if err := json.Unmarshal(d.Body, &m); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}
	if m.Type != ScanType {
		return nil // not ours: drop it
	}
	job, err := DecodeScanJob(m)
	if err != nil {
		return err
	}
	return c.handler(context.Background(), job)
}

// StreamEvent is the subset of the stack's queue.Event that the pause
// logic needs (kind "stream", state started/stopped, data.game).
type StreamEvent struct {
	Kind  string         `json:"kind"`
	State string         `json:"state"`
	Data  map[string]any `json:"data"`
}

// WatchStreams subscribes to the resourced stream events and applies the
// pause contract: a game stream (game: true) pauses the scan consumer,
// any stream end resumes it. Desktop streams (game: false) never pause.
// Like Start, it resubscribes on broker death until ctx is canceled.
func (c *Consumer) WatchStreams(ctx context.Context) error {
	for {
		err := c.watchOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("reelq: stream event subscription lost, resubscribing", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Consumer) watchOnce(ctx context.Context) error {
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(c.eventExchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	// Anonymous autodelete queue: events are fire-and-forget, and a
	// missed event is covered by the startup /state poll plus resourced's
	// keepalive re-emitting the hold.
	q, err := ch.QueueDeclare("", false, true, false, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(q.Name, "stream.#", c.eventExchange, false, nil); err != nil {
		return err
	}
	events, err := ch.Consume(q.Name, "", false, true, false, false, nil)
	if err != nil {
		return err
	}
	for e := range events {
		var ev StreamEvent
		if json.Unmarshal(e.Body, &ev) != nil {
			continue
		}
		switch ev.State {
		case "started":
			if game, _ := ev.Data["game"].(bool); game {
				slog.Info("reelq: game stream started — pausing the scan queue")
				c.Pause()
			}
		case "stopped":
			slog.Info("reelq: stream ended — resuming the scan queue")
			c.Resume()
		}
	}
	return errors.New("stream event channel closed")
}

// StreamActive polls resourced's GET /state and reports the game hold.
// This is the catch for a reeld that boots mid-game: the stream events
// only reach subscribers that were listening when the transition fired
// (resourced re-emits the initial state on its own boot, not on a new
// subscriber's), so reeld polls once at startup before it starts
// consuming. A resourced that is down is not an error (fail open, like
// the stack's lease broker): the event subscription drives later pauses.
func StreamActive(ctx context.Context, resourcedURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resourcedURL+"/state", nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var st struct {
		StreamActive bool `json:"stream_active"`
	}
	if json.NewDecoder(res.Body).Decode(&st) != nil {
		return false
	}
	return st.StreamActive
}
