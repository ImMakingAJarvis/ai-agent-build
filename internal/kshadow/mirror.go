package kshadow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/segmentio/kafka-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
)

// Mirror is the dual-write consumer: it drains the kq.mirror tap queue and
// writes every delivery to the same-named Kafka topic with header
// source=amqp, acking only after the Kafka write succeeded (a message that
// cannot be written is requeued and retried — it never leaves the shadow
// un-written, which is the whole point of the phase-1 gate).
type Mirror struct {
	amqpURL     string
	kafka       *kq.Client
	state       *State
	statePath   string
	admin       *Admin
	log         *log.Logger
	sampleEvery int // read-back one of every Nth write (0 = no sampling)

	mu          sync.Mutex
	writers     map[string]*kq.Writer
	lastWM      map[string]time.Time // per-queue last watermark write
	lastSave    time.Time
	baseBackoff map[string]time.Time // per-queue last failed baseline attempt
}

// NewMirror builds a Mirror. statePath is where the accounting survives
// restarts; admin may be nil (parity then falls back to tap counts).
func NewMirror(amqpURL string, kafka *kq.Client, state *State, statePath string, admin *Admin, sampleEvery int, logf *log.Logger) *Mirror {
	if logf == nil {
		logf = log.Default()
	}
	return &Mirror{
		amqpURL:     amqpURL,
		kafka:       kafka,
		state:       state,
		statePath:   statePath,
		admin:       admin,
		log:         logf,
		sampleEvery: sampleEvery,
		writers:     map[string]*kq.Writer{},
		lastWM:      map[string]time.Time{},
		baseBackoff: map[string]time.Time{},
	}
}

// Run consumes until ctx is done. It reconnects the AMQP session (and the
// queue declaration is idempotent-durable) and flushes watermarks + state
// on the way out.
func (m *Mirror) Run(ctx context.Context) error {
	defer m.flushAll(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.runSession(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.log.Printf("mirror: session: %v (reconnecting in 5s)", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (m *Mirror) runSession(ctx context.Context) error {
	conn, err := amqp.Dial(m.amqpURL)
	if err != nil {
		return fmt.Errorf("dial amqp: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	// kshadow's own tap: durable, no exclusive, no auto-delete, and —
	// deliberately — no dead-letter exchange, so a message that cannot be
	// mirrored waits on the mirror queue instead of leaking into dead.
	if _, err := ch.QueueDeclare(MirroredQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare %s: %w", MirroredQueue, err)
	}
	for _, ex := range []string{"tasks", "events"} {
		if err := ch.QueueBind(MirroredQueue, "#", ex, false, nil); err != nil {
			return fmt.Errorf("bind %s -> %s: %w", MirroredQueue, ex, err)
		}
	}
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("qos: %w", err)
	}
	deliveries, err := ch.Consume(MirroredQueue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	m.log.Printf("mirror: consuming %s (bound # on tasks + events)", MirroredQueue)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			m.handle(ctx, ch, d)
		}
	}
}

// handle mirrors one delivery. It never returns an error: the live path is
// upstream of the tap, and a write failure is retried by requeue, not by
// failing the session.
func (m *Mirror) handle(ctx context.Context, ch *amqp.Channel, d amqp.Delivery) {
	attr := attribute(d.RoutingKey)
	q := m.state.Queue(attr.Queue)

	// Capture the management baselines on first sight — before counting
	// this message — so "published since baseline" includes it, exactly
	// matching the tap's seq. (If the broker later restarts, its counters
	// reset below the baseline and parity's window reads as "since the
	// restart".) Queues that do not exist in mgmt — the logical events
	// aggregate has no AMQP queue — back off 60s instead of 404ing on
	// every delivery; parity shows CONSUMED "-" for them, which is the
	// honest answer.
	if m.admin != nil {
		m.mu.Lock()
		fresh := q.BaselinePub == 0
		backed := !m.baseBackoff[attr.Queue].IsZero() && time.Since(m.baseBackoff[attr.Queue]) < 60*time.Second
		m.mu.Unlock()
		if fresh && !backed {
			if st, err := m.admin.QueueStats(ctx, attr.Queue); err == nil {
				m.mu.Lock()
				q.BaselinePub = st.Published
				q.BaselineDeliver = st.Delivered
				m.mu.Unlock()
			} else {
				m.mu.Lock()
				m.baseBackoff[attr.Queue] = time.Now()
				m.mu.Unlock()
			}
		}
	}

	m.mu.Lock()
	q.Exchange = attr.Exchange
	q.Seq++
	id := messageID(d.MessageId, d.Body)
	q.LastID = id
	q.LastAt = time.Now().UTC()
	seq := q.Seq
	m.mu.Unlock()

	// Write with retries: Kafka may be coming up while RabbitMQ already
	// has traffic. Six tries × 5s covers a broker restart; then requeue
	// and let the redelivery (after other messages) retry it.
	topic := ""
	partition := -1
	var offset int64
	for attempt := 0; attempt < 6; attempt++ {
		var err error
		topic, partition, offset, err = m.write(ctx, attr, d, id, seq)
		if err == nil {
			break
		}
		m.log.Printf("mirror: write %s/%s (attempt %d): %v", attr.Queue, id, attempt+1, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	if partition < 0 {
		_ = d.Nack(false, true) // requeue; the message stays in the shadow
		return
	}

	if m.sampleEvery > 0 && (seq == 1 || seq%int64(m.sampleEvery) == 0) {
		m.readback(ctx, attr.Queue, topic, partition, offset, d.Body)
	}

	m.mu.Lock()
	q.Written++
	m.saveLocked()
	m.watermarkLocked(ctx, attr, id, seq)
	m.mu.Unlock()
	_ = d.Ack(false)
}

// topicFor maps an attribution to its Kafka topic: the queue name on
// tasks, the single events topic on events.
func topicFor(attr Attribution) string {
	if attr.Exchange == "events" {
		return EventsQueue
	}
	return attr.Queue
}

// write produces the mirrored record to the topic for the queue and
// returns the topic, partition and offset the record landed on.
func (m *Mirror) write(ctx context.Context, attr Attribution, d amqp.Delivery, id string, seq int64) (string, int, int64, error) {
	topic := topicFor(attr)
	w, err := m.writer(ctx, topic)
	if err != nil {
		return topic, -1, -1, err
	}
	headers := []kafka.Header{
		{Key: "source", Value: []byte("amqp")},
		{Key: "amqp-exchange", Value: []byte(attr.Exchange)},
		{Key: "amqp-routing-key", Value: []byte(d.RoutingKey)},
		{Key: "amqp-message-id", Value: []byte(id)},
		{Key: "seq", Value: []byte(fmt.Sprintf("%d", seq))},
		{Key: "ts", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	}
	part, off, err := w.Send(ctx, []byte(attr.Queue), d.Body, headers)
	return topic, part, off, err
}

// readback verifies one sampled write: fetch the record back at its offset
// and compare bodies byte-for-byte. A difference is counted (and logged) —
// it is the mismatch parity reports. queue names the source queue (the
// mismatch is accounted there); topic is where the record was written.
func (m *Mirror) readback(ctx context.Context, queue, topic string, partition int, offset int64, want []byte) {
	conn, err := m.kafka.Conn(ctx, topic, partition)
	if err != nil {
		m.log.Printf("mirror: readback %s p%d: dial: %v", topic, partition, err)
		return
	}
	defer conn.Close()
	if _, err := conn.Seek(offset, io.SeekStart); err != nil {
		m.log.Printf("mirror: readback %s p%d @%d: seek: %v", topic, partition, offset, err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	rec, err := conn.ReadMessage(1 << 20)
	if err != nil {
		m.log.Printf("mirror: readback %s p%d @%d: read: %v", topic, partition, offset, err)
		return
	}
	if string(rec.Value) != string(want) {
		m.mu.Lock()
		m.state.Queue(queue).Mismatch++
		m.saveLocked()
		m.mu.Unlock()
		m.log.Printf("mirror: MISMATCH %s p%d @%d: body differs from the AMQP delivery", topic, partition, offset)
	}
}

// watermarkLocked records the queue's watermark (kq.watermark) when the
// throttle (5s) has passed. Called with m.mu held.
func (m *Mirror) watermarkLocked(ctx context.Context, attr Attribution, id string, seq int64) {
	const throttle = 5 * time.Second
	now := time.Now()
	if last, ok := m.lastWM[attr.Queue]; ok && now.Sub(last) < throttle {
		return
	}
	m.lastWM[attr.Queue] = now
	q := m.state.Queue(attr.Queue)
	wm := watermark{Queue: attr.Queue, Exchange: attr.Exchange, MessageID: id, Seq: seq, At: now.UTC()}
	b, err := json.Marshal(wm)
	if err != nil {
		return
	}
	w, err := m.writer(ctx, WatermarkTopic)
	if err != nil {
		m.log.Printf("mirror: watermark writer: %v", err)
		return
	}
	if _, _, err := w.Send(ctx, []byte(attr.Queue), b, nil); err != nil {
		m.log.Printf("mirror: watermark %s: %v", attr.Queue, err)
		return
	}
	q.WatermarkSeq = seq
}

// writer returns the persistent writer for a topic, creating it (and the
// topic, if the broker does not have it yet — on-demand task queues
// mirror before any init knows them) on first use. The mirror has one
// delivery worker, so callers need no lock.
func (m *Mirror) writer(ctx context.Context, topic string) (*kq.Writer, error) {
	if w, ok := m.writers[topic]; ok {
		return w, nil
	}
	w, err := m.kafka.Writer(ctx, topic)
	if err != nil {
		return nil, err
	}
	m.writers[topic] = w
	return w, nil
}

// flushAll writes a final watermark + state + stats for every queue (the
// throttles must not hold the last message's id on rollback).
func (m *Mirror) flushAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, q := range m.state.Queues {
		if q.Seq > q.WatermarkSeq {
			wm := watermark{Queue: name, Exchange: q.Exchange, MessageID: q.LastID, Seq: q.Seq, At: time.Now().UTC()}
			b, err := json.Marshal(wm)
			if err != nil {
				continue
			}
			if w, err := m.writer(ctx, WatermarkTopic); err == nil {
				if _, _, err := w.Send(ctx, []byte(name), b, nil); err == nil {
					q.WatermarkSeq = q.Seq
				}
			}
		}
	}
	m.saveLocked()
}

// saveLocked persists the state (throttled to ~2s; the file is small and
// the traffic is low). Called with m.mu held.
func (m *Mirror) saveLocked() {
	if time.Since(m.lastSave) < 2*time.Second {
		return
	}
	m.lastSave = time.Now()
	if err := m.state.Save(m.statePath); err != nil {
		m.log.Printf("mirror: save state: %v", err)
	}
}
