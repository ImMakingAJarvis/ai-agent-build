package queue

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMailboxQueueName(t *testing.T) {
	for agent, want := range map[string]string{
		"brain":        "agent.brain.inbox",
		"igpu-helper":  "agent.igpu-helper.inbox",
		"sonnet":       "agent.sonnet.inbox",
		"planner":      "agent.planner.inbox",
	} {
		got := MailboxQueueName(agent)
		if got != want {
			t.Errorf("MailboxQueueName(%q) = %q, want %q", agent, got, want)
		}
		if back := MailboxAgentFromQueue(got); back != agent {
			t.Errorf("MailboxAgentFromQueue(%q) = %q, want %q", got, back, agent)
		}
	}
}

func TestIsMailboxQueue(t *testing.T) {
	for _, q := range []string{"agent.brain.inbox", "agent.igpu-helper.inbox", "agent.remote-1.inbox"} {
		if !IsMailboxQueue(q) {
			t.Errorf("IsMailboxQueue(%q) = false, want true", q)
		}
	}
	for _, q := range []string{
		"agent.brain",          // no .inbox suffix
		"agent..inbox",         // empty agent
		"brain.inbox",          // no agent. prefix
		"agent.",               // bare prefix
		"inbox", "flow.j1", "cap.image.generate", "voice", "",
	} {
		if IsMailboxQueue(q) {
			t.Errorf("IsMailboxQueue(%q) = true, want false", q)
		}
	}
}

func TestValidMailboxAgent(t *testing.T) {
	for _, a := range []string{"brain", "igpu-helper", "sonnet", "planner", "remote-1", "a", "z9"} {
		if !ValidMailboxAgent(a) {
			t.Errorf("ValidMailboxAgent(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"", "Brain", "brain.", ".brain", "a..b", "-brain", "brain-", "a_b", "a.b", "igpu helper", "brain/helper"} {
		if ValidMailboxAgent(a) {
			t.Errorf("ValidMailboxAgent(%q) = true, want false", a)
		}
	}
}

func TestValidMailKind(t *testing.T) {
	for _, k := range []string{MailKindQuestion, MailKindAnswer, MailKindFYI, MailKindHandoff, MailKindReviewRequest} {
		if !ValidMailKind(k) {
			t.Errorf("ValidMailKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "Agent", "QUESTION", "status", "ok", "agent"} {
		if ValidMailKind(k) {
			t.Errorf("ValidMailKind(%q) = true, want false", k)
		}
	}
}

func TestMailboxEncodeDecodeRoundTrip(t *testing.T) {
	in := MailboxMessage{
		ID:        "mb-1",
		Kind:      MailKindQuestion,
		From:      "brain",
		To:        "igpu-helper",
		Subject:   "how hot is the engine",
		Body:      "the photo window is opening; check VRAM headroom",
		Corr:      "q1",
		WakeJob:   "job-123",
		BoardRef:  "EVB-37",
		CreatedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}
	b, err := EncodeMailbox(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMailbox(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestMailboxValidate(t *testing.T) {
	ok := MailboxMessage{
		ID: "mb-1", Kind: MailKindQuestion, From: "brain", To: "igpu-helper", Subject: "s",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	cases := []struct {
		name string
		m    MailboxMessage
	}{
		{"no id", MailboxMessage{Kind: MailKindQuestion, From: "brain", To: "igpu-helper", Subject: "s"}},
		{"bad kind", MailboxMessage{ID: "x", Kind: "greeting", From: "brain", To: "igpu-helper", Subject: "s"}},
		{"kind agent is not a mail kind", MailboxMessage{ID: "x", Kind: KindAgent, From: "brain", To: "igpu-helper", Subject: "s"}},
		{"bad from", MailboxMessage{ID: "x", Kind: MailKindQuestion, From: "Brain", To: "igpu-helper", Subject: "s"}},
		{"bad to", MailboxMessage{ID: "x", Kind: MailKindQuestion, From: "brain", To: "igpu helper", Subject: "s"}},
		{"no subject", MailboxMessage{ID: "x", Kind: MailKindQuestion, From: "brain", To: "igpu-helper"}},
	}
	for _, c := range cases {
		if err := c.m.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", c.name)
		}
	}
}

// TestIsMailboxBody is the switchboard's decode-order guarantee: the
// mailbox branch must fire only for mailbox envelopes, and never for the
// other two body shapes the switchboard sees (Result, generic Message).
func TestIsMailboxBody(t *testing.T) {
	mail, _ := EncodeMailbox(MailboxMessage{
		ID: "mb-1", Kind: MailKindQuestion, From: "brain", To: "igpu-helper", Subject: "s",
	})
	if !IsMailboxBody(mail) {
		t.Error("IsMailboxBody(mailbox) = false, want true")
	}

	res, _ := EncodeResult(Result{CorrelationID: "c1", Status: ResultOK, Summary: "done"})
	if IsMailboxBody(res) {
		t.Error("IsMailboxBody(result) = true, want false")
	}

	msg, _ := json.Marshal(Message{ID: "m1", Kind: KindAgent, Title: "a job", CorrelationID: "c2"})
	if IsMailboxBody(msg) {
		t.Error("IsMailboxBody(message) = true, want false")
	}

	// A bare object with a kind but no recipient is not a mailbox.
	if IsMailboxBody([]byte(`{"kind":"question","id":"x"}`)) {
		t.Error("IsMailboxBody(kind, no to) = true, want false")
	}
	// A recipient but an unknown kind is not a mailbox either.
	if IsMailboxBody([]byte(`{"to":"brain","kind":"greeting"}`)) {
		t.Error("IsMailboxBody(to, bad kind) = true, want false")
	}
}
