package kq

// dead.go: the dead-letter topic helpers (plan §3.3). The mirror (and later
// the native consumers) publish rejected/expired messages to the `dead`
// topic with x-death-* headers. The log is append-only, so "drop" is a
// marker record on kq.dead.dropped that the list views filter out — the
// original stays in the log (audit trail), only out of the pending lists.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// DeadTopic holds dead letters; DeadDroppedTopic marks offsets that have
// been dropped (or requeued away) so list views can filter them.
const (
	DeadTopic        = "dead"
	DeadDroppedTopic = "kq.dead.dropped"
)

// DeadRecord is one dead-letter record with its x-death-* headers decoded.
type DeadRecord struct {
	Offset   int64
	Topic    string
	Reason   string    // x-death-reason: rejected | expired
	Queue    string    // x-death-queue: the original queue
	Attempts int       // x-death-attempts
	At       string    // x-death-at
	Time     time.Time // record timestamp
	Key      string
	Value    string
	Dropped  bool // marked on kq.dead.dropped (hidden from default lists)
}

func deadHeader(m *kafka.Message, name string) string {
	for _, h := range m.Headers {
		if h.Key == name {
			return string(h.Value)
		}
	}
	return ""
}

// decodeDead turns one raw record on the dead topic into a DeadRecord.
func decodeDead(m *kafka.Message) DeadRecord {
	attempts, _ := strconv.Atoi(deadHeader(m, "x-death-attempts"))
	return DeadRecord{
		Offset:   m.Offset,
		Topic:    m.Topic,
		Reason:   deadHeader(m, "x-death-reason"),
		Queue:    deadHeader(m, "x-death-queue"),
		Attempts: attempts,
		At:       deadHeader(m, "x-death-at"),
		Time:     m.Time,
		Key:      string(m.Key),
		Value:    string(m.Value),
	}
}

// DroppedOffsets reads kq.dead.dropped and returns the marked dead-topic
// offsets (the value of each marker is the dead offset it marks).
func (c *Client) DroppedOffsets(ctx context.Context) (map[int64]bool, error) {
	dropped := map[int64]bool{}
	md, err := c.Metadata(ctx, []string{DeadDroppedTopic})
	if err != nil {
		return nil, err
	}
	if len(md.Topics) != 1 || md.Topics[0].Error != nil {
		return dropped, nil // topic never written — nothing dropped
	}
	last, err := c.LastOffsets(ctx, DeadDroppedTopic)
	if err != nil {
		return nil, err
	}
	from := last[0] - 10000
	if from < 0 {
		from = 0
	}
	msgs, err := c.ReadRange(ctx, DeadDroppedTopic, 0, from, last[0], 10000)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		if off, err := strconv.ParseInt(string(msgs[i].Value), 10, 64); err == nil {
			dropped[off] = true
		}
	}
	return dropped, nil
}

// MarkDeadDropped appends a marker for the dead record at offset.
func (c *Client) MarkDeadDropped(ctx context.Context, offset int64) error {
	_, _, err := c.Send(ctx, DeadDroppedTopic, []byte(DeadTopic), []byte(strconv.FormatInt(offset, 10)),
		[]kafka.Header{{Key: "dead-topic", Value: []byte(DeadTopic)}})
	return err
}

// ListDead returns the last n dead records (newest last), dropping the
// ones marked on kq.dead.dropped unless includeDropped.
func (c *Client) ListDead(ctx context.Context, n int, includeDropped bool) ([]DeadRecord, error) {
	if n <= 0 {
		n = 20
	}
	md, err := c.Metadata(ctx, []string{DeadTopic})
	if err != nil {
		return nil, err
	}
	if len(md.Topics) != 1 || md.Topics[0].Error != nil {
		return nil, nil // dead topic not created yet — nothing dead
	}
	last, err := c.LastOffsets(ctx, DeadTopic)
	if err != nil {
		return nil, err
	}
	// over-fetch so dropped records do not shrink the visible list
	from := last[0] - int64(n*3)
	if from < 0 {
		from = 0
	}
	var dropped map[int64]bool
	if !includeDropped {
		if dropped, err = c.DroppedOffsets(ctx); err != nil {
			return nil, err
		}
	}
	msgs, err := c.ReadRange(ctx, DeadTopic, 0, from, last[0], n*3)
	if err != nil {
		return nil, err
	}
	out := make([]DeadRecord, 0, n)
	for i := len(msgs) - 1; i >= 0 && len(out) < n; i-- {
		d := decodeDead(&msgs[i])
		if !includeDropped && dropped[d.Offset] {
			continue
		}
		d.Dropped = dropped != nil && dropped[d.Offset]
		out = append(out, d)
	}
	// return oldest-first, matching the AMQP dead list
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// DeadAt reads the single dead record at offset.
func (c *Client) DeadAt(ctx context.Context, offset int64) (*DeadRecord, error) {
	m, err := c.ReadAt(ctx, DeadTopic, 0, offset)
	if err != nil {
		return nil, err
	}
	d := decodeDead(m)
	d.Key, d.Value = string(m.Key), string(m.Value)
	return &d, nil
}

// RequeueDead republishes the dead record at offset to its original queue
// (the x-death-queue topic, recreated if it no longer exists) and marks
// the dead record as handled. It returns where the copy landed.
func (c *Client) RequeueDead(ctx context.Context, offset int64) (string, int, int64, error) {
	m, err := c.ReadAt(ctx, DeadTopic, 0, offset)
	if err != nil {
		return "", -1, -1, err
	}
	queue := deadHeader(m, "x-death-queue")
	if queue == "" {
		return "", -1, -1, fmt.Errorf("dead record %d has no x-death-queue header", offset)
	}
	md, err := c.Metadata(ctx, []string{queue})
	if err != nil {
		return "", -1, -1, err
	}
	if len(md.Topics) != 1 || md.Topics[0].Error != nil {
		// the original queue topic may have been dropped — recreate it
		if err := c.CreateTopic(ctx, queue); err != nil {
			return "", -1, -1, fmt.Errorf("recreate %s: %w", queue, err)
		}
		if md, err = c.Metadata(ctx, []string{queue}); err != nil {
			return "", -1, -1, err
		}
	}
	if md.Topics[0].Error != nil {
		return "", -1, -1, md.Topics[0].Error
	}
	p, off, err := c.Republish(ctx, queue, m, kafka.Header{Key: "kq-requeued", Value: []byte(time.Now().UTC().Format(time.RFC3339))})
	if err != nil {
		return "", -1, -1, err
	}
	if err := c.MarkDeadDropped(ctx, offset); err != nil {
		return queue, p, off, fmt.Errorf("republished to %s p%d/%d but the drop mark failed: %w", queue, p, off, err)
	}
	return queue, p, off, nil
}

// DropDead marks the dead record at offset as dropped (it stays in the log;
// the marker keeps it out of the pending lists — plan §3.3).
func (c *Client) DropDead(ctx context.Context, offset int64) error {
	if _, err := c.DeadAt(ctx, offset); err != nil {
		return err
	}
	return c.MarkDeadDropped(ctx, offset)
}
