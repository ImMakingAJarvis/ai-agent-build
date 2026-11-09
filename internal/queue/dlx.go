// Dead-lettering for the cap.* and flow.* queues (plan §3.4, EVB-10):
// nothing disappears silently. A message is either processed,
// dead-lettered with a reason, or requeued. The broker does the routing:
// a consumer rejection (nack without requeue after the delivery budget is
// exhausted — see MaxAttempts) or a TTL expiry sends the message to the
// durable "dead" queue via the jarvis.dlx fanout exchange, tagged with an
// x-death header recording the reason and the queue it came from. The
// dashboard lists dead letters and can requeue each one (back to its
// original queue) or drop it.
package queue

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DLXExchange is the fanout exchange every dead message is routed to.
const DLXExchange = "jarvis.dlx"

// DeadQueue collects dead letters. It is durable — a broker restart must
// not lose the evidence — and it must never get a dead-letter argument
// itself (that would be a loop).
const DeadQueue = "dead"

// MaxAttempts is the delivery budget a consumer gives a message on a
// cap/flow queue before giving up: on each failure the consumer
// republishes the message with BumpAttempt, and once the count reaches
// MaxAttempts it nacks without requeue, which dead-letters the message
// (reason "rejected") onto the dead queue. RabbitMQ has no broker-side
// redelivery cap (x-delivery-limit is not a valid queue argument), so
// the budget is a consumer convention — AttemptCount/BumpAttempt are the
// shared helpers, and the cap worker (EVB-19) is the first to use them.
const MaxAttempts = 3

// AttemptHeader carries the delivery count on a redelivered message.
const AttemptHeader = "x-attempts"

// AttemptCount reports how many times a consumer has already tried the
// message (0 = first delivery).
func AttemptCount(d amqp.Delivery) int {
	n, _ := toInt(d.Headers[AttemptHeader])
	return n
}

// BumpAttempt returns the message's headers with the attempt count
// incremented. AMQP cannot change headers on a nack-requeue, so a retry
// is a republish: ack the failed delivery, then publish this body back
// with the bumped header.
func BumpAttempt(d amqp.Delivery) amqp.Table {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[AttemptHeader] = int32(AttemptCount(d) + 1)
	return headers
}

// dlxArgs is the dead-lettering argument every cap.*/flow.* queue is
// declared with: rejected messages (nack without requeue) are routed to
// the jarvis.dlx exchange by the broker itself.
func dlxArgs() amqp.Table {
	return amqp.Table{
		"x-dead-letter-exchange": DLXExchange,
	}
}

// EnsureDLX declares the dead-letter plumbing (idempotent): the
// jarvis.dlx fanout exchange, the durable dead queue, and the binding
// between them. The broker also gets all three from
// rabbitmq/definitions.json, but a code path that runs before a broker
// reload still works (same pattern as the events exchange).
func (c *Conn) EnsureDLX() error {
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("dlx: %w", err)
	}
	defer ch.Close()
	if err := ch.ExchangeDeclare(DLXExchange, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("dlx: declare exchange: %w", err)
	}
	if _, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("dlx: declare queue: %w", err)
	}
	if err := ch.QueueBind(DeadQueue, "", DLXExchange, true, nil); err != nil {
		return fmt.Errorf("dlx: bind: %w", err)
	}
	return nil
}

// declareWithDLX declares name as a durable queue with the dead-letter
// args (the flow.*/cap.* shape without priority).
func (c *Conn) declareWithDLX(name string) error {
	return c.declareWithDLXArgs(name, dlxArgs())
}

// declareWithDLXArgs declares name as a durable queue with the given
// arguments (the dead-letter exchange plus anything queue-specific, like
// the cap queues' x-max-priority). A queue declared with different args —
// for instance before dead-lettering, or before the priority was added —
// answers a redeclare with AMQP 406 PRECONDITION_FAILED; it is migrated
// only when it holds nothing — delete and redeclare. A queue with messages
// or consumers keeps its old shape: deleting it would lose its messages,
// and they will drain through the old consumer anyway.
func (c *Conn) declareWithDLXArgs(name string, args amqp.Table) error {
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare(name, true, false, false, false, args); err == nil {
		return nil
	} else {
		var amqpErr *amqp.Error
		if !errors.As(err, &amqpErr) || amqpErr.Code != amqp.PreconditionFailed {
			return err
		}
	}
	// A channel exception (the 406) closes the channel — AMQP semantics —
	// so the migration runs on a fresh one.
	ch.Close()
	ch, err = c.NewChannel()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer ch.Close()
	info, ierr := ch.QueueInspect(name)
	if ierr != nil {
		return err // deleted between declare and inspect; surface the 406
	}
	if info.Messages > 0 || info.Consumers > 0 {
		return nil // keep the old queue
	}
	if _, derr := ch.QueueDelete(name, true, false, false); derr != nil {
		return derr
	}
	if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
		return err
	}
	return nil
}

// Death is one x-death entry: the broker stamps each dead letter with a
// list of them, newest first, recording every death of the message.
type Death struct {
	Count  int       // times dead-lettered (grows on requeue+redie)
	Reason string    // rejected | expired | maxlen
	Queue  string    // the queue it died on
	Time   time.Time // unix time of the death
}

// parseXDeath decodes the x-death header: a list of entries, newest
// first. Brokers encode each entry either as a table
// {count, reason, exchange, queue, time, routing-keys} (RabbitMQ 3.13+)
// or as the classic [count, reason, exchange, queue, time] list — both
// shapes are handled, and time arrives as an AMQP timestamp (time.Time)
// or a unix integer depending on the path.
func parseXDeath(headers amqp.Table) []Death {
	raw, _ := headers["x-death"].([]any)
	var out []Death
	for _, e := range raw {
		var d Death
		switch entry := e.(type) {
		case amqp.Table:
			if n, ok := toInt(entry["count"]); ok {
				d.Count = n
			}
			d.Reason, _ = entry["reason"].(string)
			d.Queue, _ = entry["queue"].(string)
			d.Time = deathTime(entry["time"])
		case []any:
			if len(entry) < 5 {
				continue
			}
			if n, ok := toInt(entry[0]); ok {
				d.Count = n
			}
			d.Reason, _ = entry[1].(string)
			d.Queue, _ = entry[3].(string)
			d.Time = deathTime(entry[4])
		default:
			continue
		}
		out = append(out, d)
	}
	return out
}

func deathTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	default:
		if n, ok := toInt(t); ok {
			return time.Unix(int64(n), 0).UTC()
		}
	}
	return time.Time{}
}

// firstDeath is the most recent death of a dead letter. A message with no
// x-death header (published straight onto the dead queue) gets an
// "unknown" death so the dashboard still shows it.
func firstDeath(d amqp.Delivery) Death {
	deaths := parseXDeath(d.Headers)
	if len(deaths) == 0 {
		return Death{Reason: "unknown"}
	}
	return deaths[0]
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	}
	return 0, false
}

// DeadLetter is one message sitting on the dead queue, as the dashboard
// sees it. The original routing key is preserved on the dead delivery;
// the original queue and reason come from the x-death header.
type DeadLetter struct {
	ID         string    `json:"id"`
	RoutingKey string    `json:"routing_key"`
	Queue      string    `json:"queue"` // original queue (x-death)
	Reason     string    `json:"reason"`
	Count      int       `json:"count"`
	DeadAt     time.Time `json:"dead_at"`
	Size       int       `json:"size"`
	Preview    string    `json:"preview,omitempty"`
}

const deadPreviewBytes = 200

// deadID is a stable id for a dead letter: the same message keeps the
// same id across refreshes (dead deliveries are only ever peeked, so
// there is no delivery tag the UI could persist).
func deadID(del amqp.Delivery, d Death) string {
	h := sha1.New()
	h.Write([]byte(del.RoutingKey))
	h.Write([]byte{0})
	h.Write([]byte(d.Queue))
	h.Write([]byte{0})
	h.Write([]byte(d.Reason))
	fmt.Fprintf(h, "%d", d.Count)
	h.Write([]byte{0})
	h.Write(del.Body)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func deadPreview(body []byte) string {
	if len(body) > deadPreviewBytes {
		body = body[:deadPreviewBytes]
	}
	s := string(body)
	if !utf8.ValidString(s) {
		return fmt.Sprintf("(%d bytes, non-UTF-8)", len(body))
	}
	return s
}

// ListDead peeks at up to max messages on the dead queue without removing
// them — Get without autoAck, then nack-requeue in reverse order (the
// same trick as PeekFlow; if the process dies mid-list the broker
// requeues the unacked messages on channel close, so nothing is lost).
// max <= 0 means the default (50).
func (c *Conn) ListDead(max int) ([]DeadLetter, error) {
	if max <= 0 {
		max = 50
	}
	if err := c.EnsureDLX(); err != nil {
		return nil, err
	}
	ch, err := c.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("dead: %w", err)
	}
	defer ch.Close()
	var (
		dels []amqp.Delivery
		out  []DeadLetter
	)
	for i := 0; i < max; i++ {
		del, ok, err := ch.Get(DeadQueue, false)
		if err != nil {
			return nil, fmt.Errorf("dead: get: %w", err)
		}
		if !ok {
			break
		}
		dels = append(dels, del)
		d := firstDeath(del)
		out = append(out, DeadLetter{
			ID:         deadID(del, d),
			RoutingKey: del.RoutingKey,
			Queue:      d.Queue,
			Reason:     d.Reason,
			Count:      d.Count,
			DeadAt:     d.Time,
			Size:       len(del.Body),
			Preview:    deadPreview(del.Body),
		})
	}
	if err := putBackDead(dels); err != nil {
		return nil, fmt.Errorf("dead: requeue: %w", err)
	}
	return out, nil
}

// takeDead pulls messages off the dead queue until it finds the one with
// id (holding the earlier ones), and returns it with the held list and
// the channel. The caller acks the target or lets the channel close, and
// puts the held ones back with putBackDead.
func (c *Conn) takeDead(id string) (amqp.Delivery, []amqp.Delivery, *amqp.Channel, error) {
	if err := c.EnsureDLX(); err != nil {
		return amqp.Delivery{}, nil, nil, err
	}
	ch, err := c.NewChannel()
	if err != nil {
		return amqp.Delivery{}, nil, nil, fmt.Errorf("dead: %w", err)
	}
	var held []amqp.Delivery
	for {
		del, ok, err := ch.Get(DeadQueue, false)
		if err != nil {
			ch.Close()
			return amqp.Delivery{}, nil, nil, fmt.Errorf("dead: get: %w", err)
		}
		if !ok {
			ch.Close()
			return amqp.Delivery{}, nil, nil, fmt.Errorf("dead: no dead letter with id %s", id)
		}
		if deadID(del, firstDeath(del)) == id {
			return del, held, ch, nil
		}
		held = append(held, del)
	}
}

func putBackDead(dels []amqp.Delivery) error {
	for i := len(dels) - 1; i >= 0; i-- {
		if err := dels[i].Nack(false, true); err != nil {
			return err
		}
	}
	return nil
}

// RequeueDead republishes the dead letter with id back to its original
// queue (same exchange, same routing key, x-death stripped so the
// requeue starts a fresh death count) and removes it from the dead queue.
// If the original queue no longer exists it is re-created first (flow and
// cap queues are declared on demand) so the message is not lost to an
// unroutable publish. On a failed republish the dead letter is NOT acked
// — it stays on the dead queue and the requeue can be retried.
func (c *Conn) RequeueDead(id string) error {
	del, held, ch, err := c.takeDead(id)
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := putBackDead(held); err != nil {
		return fmt.Errorf("dead: requeue hold: %w", err)
	}
	orig := firstDeath(del).Queue
	switch {
	case IsFlowQueue(orig):
		if err := c.EnsureFlowQueue(orig); err != nil {
			return err
		}
	case IsCapQueue(orig):
		if err := c.EnsureCapQueue(orig); err != nil {
			return err
		}
	}
	headers := amqp.Table{}
	for k, v := range del.Headers {
		if k != "x-death" {
			headers[k] = v
		}
	}
	pub := amqp.Publishing{
		Headers:         headers,
		Body:            del.Body,
		ContentType:     del.ContentType,
		ContentEncoding: del.ContentEncoding,
		DeliveryMode:    amqp.Persistent,
		Timestamp:       time.Now(),
	}
	if err := ch.PublishWithContext(context.Background(), c.Exchange(), del.RoutingKey, false, false, pub); err != nil {
		return fmt.Errorf("dead: republish to %s: %w", del.RoutingKey, err)
	}
	if err := del.Ack(false); err != nil {
		return fmt.Errorf("dead: ack: %w", err)
	}
	return nil
}

// DropDead acks (discards) the dead letter with id. The discard is
// deliberate and shows up on the dashboard — the alternative, leaving
// it on the dead queue forever, is the silent-disappearing we are
// trying to prevent.
func (c *Conn) DropDead(id string) error {
	del, held, ch, err := c.takeDead(id)
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := del.Ack(false); err != nil {
		return fmt.Errorf("dead: ack: %w", err)
	}
	if err := putBackDead(held); err != nil {
		return fmt.Errorf("dead: requeue hold: %w", err)
	}
	return nil
}
