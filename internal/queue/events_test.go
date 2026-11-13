package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// routing keys per the four families in plan §3.4.
func TestEventRoutingKey(t *testing.T) {
	tests := []struct {
		name string
		e    Event
		want string
	}{
		{"job capability", JobEvent(Message{ID: "j1", Capability: "photo"}, "started", nil), "job.photo.started"},
		{"job falls back to v1 type", JobEvent(Message{ID: "j2", Type: "voice"}, "done", nil), "job.voice.done"},
		{"agent", AgentEvent("plan-x-1", "resumed", nil), "agent.plan-x-1.resumed"},
		{"window", WindowEvent("opened", nil), "window.opened"},
		{"lease", LeaseEvent("denied", nil), "lease.denied"},
		{"stream", StreamEvent("started", nil), "stream.started"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.RoutingKey(); got != tt.want {
				t.Errorf("RoutingKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEventRoutingKey(t *testing.T) {
	tests := []struct {
		key       string
		kind      string
		subject   string
		state     string
		wantErr   bool
	}{
		{"job.photo.started", "job", "photo", "started", false},
		{"agent.plan-x-1.resumed", "agent", "plan-x-1", "resumed", false},
		{"window.opened", "window", "", "opened", false},
		{"lease.denied", "lease", "", "denied", false},
		{"stream.started", "stream", "", "started", false},
		{"stream.stopped", "stream", "", "stopped", false},
		{"job.photo", "", "", "", true},
		{"not-dotted", "", "", "", true},
		{"a.b.c.d", "", "", "", true},
	}
	for _, tt := range tests {
		kind, subject, state, err := ParseEventRoutingKey(tt.key)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseEventRoutingKey(%q): want error, got none", tt.key)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseEventRoutingKey(%q): %v", tt.key, err)
		}
		if kind != tt.kind || subject != tt.subject || state != tt.state {
			t.Errorf("ParseEventRoutingKey(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tt.key, kind, subject, state, tt.kind, tt.subject, tt.state)
		}
	}
}

// an event must survive the wire: marshal, decode, same routing key and
// payload.
func TestEventJSONRoundTrip(t *testing.T) {
	in := JobEvent(Message{ID: "job-123", Capability: "fix", Priority: 2}, "progress",
		map[string]any{"attempt": 1.0, "note": "halfway"})
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := DecodeEvent(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RoutingKey() != in.RoutingKey() {
		t.Errorf("routing key: got %q want %q", out.RoutingKey(), in.RoutingKey())
	}
	if out.JobID != "job-123" || out.State != "progress" {
		t.Errorf("payload mismatch: %+v", out)
	}
	if out.Data["note"] != "halfway" {
		t.Errorf("data: %v", out.Data)
	}
	// empty subject omits from the wire but parses back to "".
	wb, _ := json.Marshal(WindowEvent("closed", nil))
	out2, err := DecodeEvent(wb)
	if err != nil {
		t.Fatalf("decode window: %v", err)
	}
	if out2.Subject != "" || out2.RoutingKey() != "window.closed" {
		t.Errorf("window round-trip: %+v", out2)
	}
}

func TestJobEventCapabilityFallback(t *testing.T) {
	if e := JobEvent(Message{Capability: "photo"}, "started", nil); e.Subject != "photo" {
		t.Errorf("v2 capability: %q", e.Subject)
	}
	if e := JobEvent(Message{Type: "image"}, "failed", nil); e.Subject != "image" {
		t.Errorf("v1 type: %q", e.Subject)
	}
}

// Live round-trip: publish an event to the exchange, consume it from a
// temporary queue bound with #, and check routing key and body.
func TestEventPublishLive(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()
	p, err := NewEventPublisher(c, "events-test")
	if err != nil {
		t.Fatalf("NewEventPublisher: %v", err)
	}
	defer p.Close()

	qname := "events.live-test"
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	// Fresh each run so leftover messages from an earlier run cannot
	// satisfy the assertion.
	ch.QueueDelete(qname, true, false, false)
	if _, err := ch.QueueDeclare(qname, false, true, false, false, nil); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := ch.QueueBind(qname, "#", EventsExchange, false, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}

	in := JobEvent(Message{ID: "live-1", Capability: "test.ping"}, "started",
		map[string]any{"from": "events_test"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Publish(ctx, in); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Get returns once the message has been routed (fire-and-forget
	// publish is async; poll briefly).
	var del struct {
		RoutingKey string
		Body       []byte
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, ok, err := ch.Get(qname, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if ok {
			del.RoutingKey, del.Body = d.RoutingKey, d.Body
			d.Ack(false)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if del.Body == nil {
		t.Fatal("no event received on the bound queue")
	}
	if del.RoutingKey != "job.test.ping.started" {
		t.Errorf("routing key: got %q want job.test.ping.started", del.RoutingKey)
	}
	out, err := DecodeEvent(del.Body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.JobID != "live-1" || out.Source != "events-test" {
		t.Errorf("event: %+v", out)
	}
}
