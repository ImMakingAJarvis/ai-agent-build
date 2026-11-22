package kq

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// CountSource counts the records of a topic whose header key carries the
// given value — the parity check's independent shadow-side count
// (source=amqp records, plan §5). It scans the whole log from the
// log-start offset; the shadow topics are single-partition and low-volume
// (a few thousand records over 48h), so a full scan is cheap enough for a
// CLI/dashboard probe. A topic the broker does not know counts as zero
// (nothing mirrored yet).
//
// kafka-go v0.4 reads from the connection's current position, so the scan
// Seeks to the log start once and lets each fetched batch advance it.
func (c *Client) CountSource(ctx context.Context, topic, headerKey, headerValue string) (int64, error) {
	first, err := c.FirstOffsets(ctx, topic)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return 0, nil
		}
		return 0, err
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	var n int64
	for p, end := range last {
		start := first[p]
		if start >= end {
			continue
		}
		conn, err := c.Conn(ctx, topic, p)
		if err != nil {
			return 0, err
		}
		if _, err := conn.Seek(start, io.SeekStart); err != nil {
			conn.Close()
			return 0, err
		}
		for off := start; off < end; {
			// Bounds the broker's fetch wait; refresh per fetch (the
			// deadline is absolute and would expire mid-scan on long
			// topics), so it only costs anything on an empty topic.
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			batch := conn.ReadBatch(1, 1<<20)
			var got bool
			for {
				rec, err := batch.ReadMessage()
				if err == io.EOF {
					break
				}
				if err != nil {
					batch.Close()
					conn.Close()
					return 0, err
				}
				got = true
				for _, h := range rec.Headers {
					if h.Key == headerKey && string(h.Value) == headerValue {
						n++
					}
				}
				off = rec.Offset + 1
				if off >= end {
					break
				}
			}
			batch.Close()
			if off >= end {
				break
			}
			// A batch that returned nothing while records remain means the
			// fetch came back empty mid-scan: report it rather than spin.
			if !got {
				conn.Close()
				return 0, fmt.Errorf("topic %q partition %d: empty fetch at offset %d (log end %d)", topic, p, off, end)
			}
		}
		conn.Close()
	}
	return n, nil
}
