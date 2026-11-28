package api

import (
	"testing"
	"time"
)

func TestPublishAssignsIncreasingIDs(t *testing.T) {
	b := NewBroker(16)
	ch, cancel := b.Subscribe()
	defer cancel()
	for i := 0; i < 5; i++ {
		b.Publish(Event{Type: "tick", JobID: "j1"})
	}
	for i := 0; i < 5; i++ {
		select {
		case e := <-ch:
			if e.ID != int64(i+1) {
				t.Fatalf("event %d: ID = %d, want %d", i, e.ID, i+1)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

// TestSlowSubscriberDroppedNotBlocking pins the contract a publisher
// relies on: Publish must never block on a full subscriber channel, and
// the events the slow subscriber loses are dropped, not queued.
func TestSlowSubscriberDroppedNotBlocking(t *testing.T) {
	b := NewBroker(1)
	ch, cancel := b.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		b.Publish(Event{Type: "a"})
		b.Publish(Event{Type: "b"})
		b.Publish(Event{Type: "c"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber channel")
	}
	// Exactly the buffered event survives.
	select {
	case e := <-ch:
		if e.Type != "a" {
			t.Fatalf("first event = %q, want %q", e.Type, "a")
		}
	case <-time.After(time.Second):
		t.Fatal("expected the buffered event")
	}
	// And the channel is now empty: b and c were dropped.
	select {
	case e := <-ch:
		t.Fatalf("unexpected event %q", e.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := NewBroker(1)
	ch, cancel := b.Subscribe()
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel was not closed by unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not close the channel")
	}
	// A second unsubscribe must be a no-op, not a double close.
	cancel()
	// A publish after the unsubscribe must not resurrect the channel.
	b.Publish(Event{Type: "x"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("closed channel delivered an event")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestCloseEndsSubscribers covers the other way a stream ends: the
// broker closes and every subscriber's channel closes with it.
func TestCloseEndsSubscribers(t *testing.T) {
	b := NewBroker(1)
	ch, cancel := b.Subscribe()
	b.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("Close did not close the subscriber channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not close the subscriber channel")
	}
	cancel() // must stay safe after Close
	// A publish after Close is a no-op and does not panic.
	b.Publish(Event{Type: "after-close"})
	// A subscribe after Close gets an already-closed channel.
	ch2, cancel2 := b.Subscribe()
	defer cancel2()
	select {
	case _, ok := <-ch2:
		if ok {
			t.Fatal("post-Close channel was not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("post-Close channel was not closed")
	}
}
