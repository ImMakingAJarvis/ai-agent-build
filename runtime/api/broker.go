// broker.go is the runtime event stream (card ELR-96, M4.1d): a Broker
// that assigns increasing ids to published events and fans them out to
// its subscribers without ever blocking the publisher — a slow
// subscriber loses events, the loop does not stop for the UI.
package api

import (
	"encoding/json"
	"sync"
)

// Event is one message on the stream (M4.1d): a typed event tied to a
// job (and optionally one of its ops) with a JSON payload.
type Event struct {
	ID    int64           `json:"id"`
	Type  string          `json:"type"`
	JobID string          `json:"job_id,omitempty"`
	OpID  string          `json:"op_id,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Broker fans events out to its subscribers. A publisher never blocks:
// a subscriber whose channel is full drops the event. IDs start at 1 and
// increase by one per published event.
type Broker struct {
	mu     sync.Mutex
	next   int64
	buffer int
	subs   map[chan Event]struct{}
	closed bool
}

// NewBroker returns a Broker whose subscriber channels hold up to
// buffer events (a non-positive buffer is treated as 1).
func NewBroker(buffer int) *Broker {
	if buffer < 1 {
		buffer = 1
	}
	return &Broker{next: 1, buffer: buffer, subs: make(map[chan Event]struct{})}
}

// Publish assigns the next id to e and sends it to every subscriber. It
// never blocks: a subscriber whose channel is full drops the event. A
// publish after Close is a no-op.
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	e.ID = b.next
	b.next++
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // full: this subscriber is slow and drops the event.
		}
	}
}

// Subscribe returns a channel that receives every event published from
// now on, and a function that unsubscribes and closes the channel (safe
// to call more than once). A subscribe after Close returns an
// already-closed channel and a no-op cancel.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	ch := make(chan Event, b.buffer)
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// subsLen returns the number of active subscribers (test support: it is
// how a test waits for a handler to have subscribed before it reads the
// recorder, whose header writes the handler made just before).
func (b *Broker) subsLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Close ends the stream: every subscriber's channel is closed (so their
// read loops see the end), later publishes are no-ops, and later
// subscribes get an already-closed channel. Safe to call more than once.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subs
	b.subs = make(map[chan Event]struct{})
	b.mu.Unlock()
	for ch := range subs {
		close(ch)
	}
}
