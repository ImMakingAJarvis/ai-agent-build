package kq

// modify.go: the mutation side of jarvis-kq (plan §4): seek (external
// OffsetCommit), pop (group read + commit), control records on kq.control
// (the phase-3 consumer SDK honours them, and as log records they survive
// restarts), and read-slice/republish helpers for replay, requeue and edit.
// Kafka is append-only, so "edit" and "requeue" produce new records next to
// the original; the original is never rewritten.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// ControlTopic carries group control records (key = consumer group).
const ControlTopic = "kq.control"

// ControlAction is one kq.control record.
type ControlAction struct {
	Action string    `json:"action"` // pause | resume | throttle | rebalance
	Rate   float64   `json:"rate,omitempty"`
	Topic  string    `json:"topic,omitempty"`
	At     time.Time `json:"at"`
}

// PublishControl appends one control record for the group. It is a log
// record, so the control sticks across consumer restarts; the consumer
// SDK (phase 3) applies it on every poll.
func (c *Client) PublishControl(ctx context.Context, group string, a ControlAction) error {
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, _, err = c.Send(ctx, ControlTopic, []byte(group), b, nil)
	return err
}

// GroupControlState returns the group's state and member count (for the
// rebalance command's before/after report).
type GroupControlState struct {
	GroupID    string
	GroupState string
	Members    int
}

// DescribeGroup returns one group's state via DescribeGroups.
func (c *Client) DescribeGroup(ctx context.Context, group string) (*GroupControlState, error) {
	res, err := c.raw().DescribeGroups(ctx, &kafka.DescribeGroupsRequest{GroupIDs: []string{group}})
	if err != nil {
		return nil, err
	}
	for _, g := range res.Groups {
		if g.Error != nil {
			return nil, fmt.Errorf("group %s: %w", group, g.Error)
		}
		if g.GroupID != group {
			continue
		}
		return &GroupControlState{GroupID: g.GroupID, GroupState: g.GroupState, Members: len(g.Members)}, nil
	}
	return nil, fmt.Errorf("group %s not found", group)
}

// Seek commits offset for (group, topic, partition) without joining the
// group: an external commit (GenerationID -1, empty MemberID) — the same
// path kafka-consumer-groups.sh --offset uses.
func (c *Client) Seek(ctx context.Context, group, topic string, partition int, offset int64) error {
	if offset < 0 {
		return fmt.Errorf("offset must be >= 0 (got %d)", offset)
	}
	res, err := c.raw().OffsetCommit(ctx, &kafka.OffsetCommitRequest{
		GroupID:      group,
		GenerationID: -1,
		MemberID:     "",
		Topics: map[string][]kafka.OffsetCommit{
			topic: {{Partition: partition, Offset: offset}},
		},
	})
	if err != nil {
		return err
	}
	for _, p := range res.Topics[topic] {
		if p.Error != nil {
			return fmt.Errorf("seek %s/%s p%d: %w", group, topic, p.Partition, p.Error)
		}
	}
	return nil
}

// Pop consumes up to n messages for group from topic (partition 0) and
// commits the new offset as it goes — the Kafka version of basic.get with
// requeue=false. It returns immediately when the group is caught up.
func (c *Client) Pop(ctx context.Context, group, topic string, n int) ([]kafka.Message, error) {
	if n <= 0 {
		n = 10
	}
	committed, err := c.GroupOffset(ctx, group)
	if err != nil {
		return nil, err
	}
	commit := int64(0)
	hasCommit := false
	for _, p := range committed[topic] {
		commit, hasCommit = p.CommittedOffset, true
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	end := last[0]
	start := int64(0)
	if hasCommit {
		start = commit
	}
	if start >= end {
		return nil, nil
	}

	// CommitInterval 0 = commit after every message (useSyncCommits);
	// StartOffset applies while the group has no committed offset.
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        c.Brokers,
		GroupID:        group,
		GroupTopics:    []string{topic},
		StartOffset:    start,
		CommitInterval: 0,
		MinBytes:       1,
		MaxBytes:       10e6,
		ReadBackoffMax: 500 * time.Millisecond,
	})
	defer r.Close()

	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out []kafka.Message
	for len(out) < n {
		msg, err := r.ReadMessage(readCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				break
			}
			return out, err
		}
		out = append(out, msg)
	}
	return out, nil
}

// ReadRange reads the records in [from, to) of one partition, at most max.
// from must be an existing offset (use FirstOffsets); to may be the log end
// (the read stops at the log end or max, whichever comes first).
//
// The conn's deadline comes from ctx: a fetch with zero MaxWait blocks until
// the conn deadline, so a partition at the log end would otherwise hold a
// bounded read for a full fixed 30s — past the caller's context, so that the
// next call in the sequence (ListDead's second read) dialed an already-expired
// context and failed with a lookup timeout.
func (c *Client) ReadRange(ctx context.Context, topic string, partition int, from, to int64, max int) ([]kafka.Message, error) {
	if max <= 0 {
		max = 500
	}
	if from < 0 || to <= from {
		return nil, nil
	}
	conn, err := c.Conn(ctx, topic, partition)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, err := conn.Seek(from, kafka.SeekAbsolute|kafka.SeekDontCheck); err != nil {
		return nil, fmt.Errorf("seek to %d: %w", from, err)
	}
	var out []kafka.Message
	for len(out) < max {
		// explicit MaxWait: at the log end the broker answers the empty
		// fetch after ~1s (RequestTimedOut) instead of the fetch running
		// to the conn deadline.
		batch := conn.ReadBatchWith(kafka.ReadBatchConfig{
			MinBytes: 1, MaxBytes: 10e6, MaxWait: time.Second,
		})
		msg, err := batch.ReadMessage()
		cerr := batch.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, kafka.OffsetOutOfRange) || errors.Is(err, io.EOF) ||
				errors.Is(err, kafka.RequestTimedOut) ||
				strings.Contains(err.Error(), "offset out of range") {
				break // stopped at the log end (from was stale) or the read timed out
			}
			return out, err
		}
		if msg.Offset >= to {
			break
		}
		out = append(out, msg)
	}
	return out, nil
}

// ReadAt reads the single record at offset (the edit/requeue-by-offset path).
func (c *Client) ReadAt(ctx context.Context, topic string, partition int, offset int64) (*kafka.Message, error) {
	msgs, err := c.ReadRange(ctx, topic, partition, offset, offset+1, 1)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 || msgs[0].Offset != offset {
		return nil, fmt.Errorf("no record at %s/%d offset %d", topic, partition, offset)
	}
	return &msgs[0], nil
}

// Republish writes a copy of m to its own topic (or dest when non-empty),
// keeping key and headers, with extra headers appended. It returns the
// partition and offset the copy landed on.
func (c *Client) Republish(ctx context.Context, dest string, m *kafka.Message, extra ...kafka.Header) (int, int64, error) {
	if dest == "" {
		dest = m.Topic
	}
	hdrs := append([]kafka.Header{}, m.Headers...)
	hdrs = append(hdrs, extra...)
	return c.Send(ctx, dest, m.Key, m.Value, hdrs)
}

// FindByCorrelation scans the last scan records of topic (partition 0) for
// one whose key or body JSON (id / correlation_id / job_id) equals corr —
// the requeue-by-corr path for records the caller only knows by id.
func (c *Client) FindByCorrelation(ctx context.Context, topic, corr string, scan int) (*kafka.Message, error) {
	if scan <= 0 {
		scan = 1000
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	end := last[0]
	from := end - int64(scan)
	if from < 0 {
		from = 0
	}
	msgs, err := c.ReadRange(ctx, topic, 0, from, end, scan)
	if err != nil {
		return nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := &msgs[i]
		if string(m.Key) == corr {
			return m, nil
		}
		var probe struct {
			ID            string `json:"id"`
			CorrelationID string `json:"correlation_id"`
			JobID         string `json:"job_id"`
		}
		if json.Unmarshal(m.Value, &probe) == nil {
			if probe.ID == corr || probe.CorrelationID == corr || probe.JobID == corr {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("no record matching %q in the last %d of %s", corr, scan, topic)
}
