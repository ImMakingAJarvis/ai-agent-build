package kq

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
)

// Writer is a persistent single-topic producer. Client.Send opens a fresh
// kafka.Writer per message (right for a CLI; far too heavy for the
// mirror's steady stream), so the kshadow mirror keeps one Writer per
// topic instead.
type Writer struct {
	c *Client
	w *kafka.Writer
}

// Writer returns (creating if needed, including the topic under
// auto-create rules — one partition, RF 1) a persistent writer for the
// topic.
func (c *Client) Writer(ctx context.Context, topic string) (*Writer, error) {
	md, err := c.Metadata(ctx, []string{topic})
	if err != nil {
		return nil, err
	}
	if len(md.Topics) != 1 || md.Topics[0].Error != nil {
		if err := c.CreateTopic(ctx, topic); err != nil {
			return nil, err
		}
		md, err = c.Metadata(ctx, []string{topic})
		if err != nil {
			return nil, err
		}
	}
	if md.Topics[0].Error != nil {
		return nil, md.Topics[0].Error
	}
	return &Writer{c: c, w: &kafka.Writer{
		Addr:         kafka.TCP(c.Brokers[0]),
		Topic:        topic,
		RequiredAcks: kafka.RequireAll,
		BatchSize:    1, // no batching: each write lands and reports its offset
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: c.Timeout,
		MaxAttempts:  3,
	}}, nil
}

// Send produces one record and returns the partition and offset it landed
// on (kafka-go fills them in on success — exact, no log-end guessing).
func (w *Writer) Send(ctx context.Context, key, value []byte, headers []kafka.Header) (int, int64, error) {
	msg := kafka.Message{Key: key, Value: value, Headers: headers}
	if err := w.w.WriteMessages(ctx, msg); err != nil {
		return 0, -1, err
	}
	return int(msg.Partition), msg.Offset, nil
}

// Close flushes and closes the underlying writer.
func (w *Writer) Close() error {
	return w.w.Close()
}
