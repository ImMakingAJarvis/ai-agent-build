package queue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoopEventEncodeDecode(t *testing.T) {
	in := LoopEvent{
		ID:      "ev-123-abcdef",
		Type:    EvOpDone,
		JobID:   "job-1",
		OpID:    "op-7",
		Payload: json.RawMessage(`{"tool":"shell","exit":0}`),
		At:      time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC),
	}
	b, err := EncodeLoopEvent(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeLoopEvent(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != in.ID || out.Type != in.Type || out.JobID != in.JobID || out.OpID != in.OpID {
		t.Errorf("round trip changed fields: got %+v want %+v", out, in)
	}
	if !out.At.Equal(in.At) {
		t.Errorf("round trip changed At: got %v want %v", out.At, in.At)
	}
	if string(out.Payload) != string(in.Payload) {
		t.Errorf("round trip changed Payload: got %s want %s", out.Payload, in.Payload)
	}
	// omitempty: an event without JobID/OpID/Payload encodes without
	// those keys, and decodes back to the zero values.
	sparse, err := EncodeLoopEvent(LoopEvent{ID: "ev-1-abcdef", Type: EvUserMessage, At: in.At})
	if err != nil {
		t.Fatalf("encode sparse: %v", err)
	}
	if string(sparse) != `{"id":"ev-1-abcdef","type":"user.message","at":"2026-11-25T22:00:00Z"}` {
		t.Errorf("sparse encoding: %s", sparse)
	}
	var back LoopEvent
	if err := json.Unmarshal(sparse, &back); err != nil {
		t.Fatalf("decode sparse: %v", err)
	}
	if back.JobID != "" || back.OpID != "" || len(back.Payload) != 0 {
		t.Errorf("sparse decode should have empty optional fields: %+v", back)
	}
}

func TestLoopEventDecodeRejectsEmpty(t *testing.T) {
	cases := map[string][]byte{
		"empty id":   []byte(`{"id":"","type":"op.done","at":"2026-11-25T22:00:00Z"}`),
		"empty type": []byte(`{"id":"ev-1-abcdef","type":"","at":"2026-11-25T22:00:00Z"}`),
		"missing id": []byte(`{"type":"op.done","at":"2026-11-25T22:00:00Z"}`),
		"missing type": []byte(`{"id":"ev-1-abcdef","at":"2026-11-25T22:00:00Z"}`),
		"not json":    []byte(`nope`),
	}
	for name, body := range cases {
		if _, err := DecodeLoopEvent(body); err == nil {
			t.Errorf("%s: DecodeLoopEvent accepted %s", name, body)
		}
	}
	// The happy path still works: a valid body decodes.
	if _, err := DecodeLoopEvent([]byte(`{"id":"ev-1-abcdef","type":"op.done","at":"2026-11-25T22:00:00Z"}`)); err != nil {
		t.Errorf("valid body rejected: %v", err)
	}
}

func TestNewLoopEventUniqueIDs(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ev := NewLoopEvent(EvTaskSubmitted, "job-1")
		if ev.ID == "" {
			t.Fatalf("event %d: empty id", i)
		}
		if !strings.HasPrefix(ev.ID, "ev-") {
			t.Fatalf("event %d: id %q does not start with \"ev-\"", i, ev.ID)
		}
		if seen[ev.ID] {
			t.Fatalf("event %d: duplicate id %q", i, ev.ID)
		}
		seen[ev.ID] = true
		if ev.Type != EvTaskSubmitted {
			t.Errorf("event %d: type %q, want %q", i, ev.Type, EvTaskSubmitted)
		}
		if ev.JobID != "job-1" {
			t.Errorf("event %d: job id %q, want %q", i, ev.JobID, "job-1")
		}
		if ev.At.IsZero() || !ev.At.Equal(ev.At.UTC()) {
			t.Errorf("event %d: At %v is not UTC", i, ev.At)
		}
	}
}
