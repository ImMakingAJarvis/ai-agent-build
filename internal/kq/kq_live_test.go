package kq

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// liveClient returns a client against the local broker (KAFKA_BROKERS,
// default 127.0.0.1:9092), skipping the test when the broker is
// unreachable — like the other live tests, so `go test ./...` stays green
// off this machine.
func liveClient(t *testing.T) *Client {
	t.Helper()
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Topics(ctx); err != nil {
		t.Skipf("Kafka broker unavailable: %v", err)
	}
	return c
}

func liveTopic(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("kq.live.%d", time.Now().UnixNano())
}

// TestLiveRoundTrip exercises the whole v1 surface on a scratch topic:
// send (auto-create), depth, record read, purge, drop.
func TestLiveRoundTrip(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := liveTopic(t)

	var offsets []int64
	for i, v := range []string{"a", "b", "c"} {
		_, off, err := c.Send(ctx, topic, []byte(fmt.Sprintf("k%d", i)), []byte(v), nil)
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		offsets = append(offsets, off)
	}
	if offsets[0] != 0 || offsets[2] != 2 {
		t.Fatalf("expected offsets 0..2, got %v", offsets)
	}

	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.FirstOffsets(ctx, topic)
	if err != nil {
		t.Fatal(err)
	}
	if last[0] != 3 || first[0] != 0 {
		t.Fatalf("depth: first=%d last=%d, want 0/3", first[0], last[0])
	}

	conn, err := c.Conn(ctx, topic, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Seek(0, kafka.SeekAbsolute); err != nil {
		t.Fatal(err)
	}
	m, err := conn.ReadMessage(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Value) != "a" || m.Offset != 0 {
		t.Fatalf("first record: offset=%d value=%q", m.Offset, m.Value)
	}
	conn.Close()

	if _, err := c.Purge(ctx, topic); err != nil {
		t.Fatalf("purge: %v", err)
	}
	first, err = c.FirstOffsets(ctx, topic)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != 3 {
		t.Fatalf("after purge log start=%d, want 3", first[0])
	}

	if err := c.DeleteTopic(ctx, topic); err != nil {
		t.Fatalf("drop: %v", err)
	}
	// DeleteTopics returns once the delete is accepted; the broker removes
	// the topic asynchronously, so poll until it is gone.
	deadline := time.Now().Add(15 * time.Second)
	for {
		topics, err := c.Topics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		gone := true
		for _, tp := range topics {
			if tp.Name == topic {
				gone = false
			}
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %q still listed 15s after drop", topic)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestLiveGroupsAndFetch checks the group read side: a broker answers
// ListGroups, and OffsetFetch of a group that never committed is empty
// (not an error) — which is what `jarvis-kq lag` turns into full-depth lag.
func TestLiveGroupsAndFetch(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	group := "kq-live-" + fmt.Sprint(time.Now().UnixNano())

	if _, err := c.Groups(ctx); err != nil {
		t.Fatalf("list groups: %v", err)
	}
	committed, err := c.GroupOffset(ctx, group)
	if err != nil {
		t.Fatalf("offset fetch of unknown group: %v", err)
	}
	if len(committed) != 0 {
		t.Fatalf("unknown group has committed state: %+v", committed)
	}
}
