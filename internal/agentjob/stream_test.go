package agentjob

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestStreamPublisherLines(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	p := NewStreamPublisher("redis://"+mr.Addr(), "job1")
	if p == nil {
		t.Fatal("NewStreamPublisher returned nil")
	}
	// Line one arrives in two writes; line two in one.
	p.Write([]byte(`{"type":"system","ok"`))
	p.Write([]byte(":true}\n" + `{"type":"assistant","x":1}` + "\n"))
	p.Close()

	entries, err := rdb.XRange(context.Background(), StreamKey("job1"), "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%d", len(entries))
	}
	if s, _ := entries[0].Values["data"].(string); s != `{"type":"system","ok":true}`+"\n" {
		t.Errorf("line0 %q", s)
	}
	if s, _ := entries[1].Values["data"].(string); s != `{"type":"assistant","x":1}`+"\n" {
		t.Errorf("line1 %q", s)
	}
	if ttl, err := rdb.TTL(context.Background(), StreamKey("job1")).Result(); err != nil || ttl <= 0 {
		t.Errorf("stream ttl=%v err=%v", ttl, err)
	}
	if p.Drops() != 0 {
		t.Errorf("drops=%d", p.Drops())
	}
}

func TestStreamPublisherDisabledAndUnreachable(t *testing.T) {
	if NewStreamPublisher("off", "x") != nil {
		t.Error("off should return nil")
	}
	if NewStreamPublisher("redis://127.0.0.1:1", "x") != nil {
		t.Error("unreachable should return nil")
	}
}

func TestStreamPublisherMaxLen(t *testing.T) {
	mr := miniredis.RunT(t)
	p := NewStreamPublisher("redis://"+mr.Addr(), "big")
	if p == nil {
		t.Fatal("NewStreamPublisher returned nil")
	}
	// More lines than the cap; the writer outruns the publisher and may
	// drop, but what lands in Redis must stay under the cap.
	for i := 0; i < streamMaxLen+50; i++ {
		p.Write([]byte(fmt.Sprintf("line %06d\n", i)))
	}
	p.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	l, err := rdb.XLen(context.Background(), StreamKey("big")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if l > streamMaxLen {
		t.Errorf("xlen=%d > %d", l, streamMaxLen)
	}
}
