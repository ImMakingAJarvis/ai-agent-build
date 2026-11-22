package kshadow

import "testing"

func TestAttribute(t *testing.T) {
	cases := []struct {
		key  string
		want Attribution
	}{
		// task-family keys are their own queue names
		{key: "voice", want: Attribution{"tasks", "voice"}},
		{key: "fix", want: Attribution{"tasks", "fix"}},
		{key: "image.p10", want: Attribution{"tasks", "image.p10"}},
		{key: "coding.p5", want: Attribution{"tasks", "coding.p5"}},
		{key: "background", want: Attribution{"tasks", "background"}},
		{key: "review.sonnet", want: Attribution{"tasks", "review.sonnet"}},
		{key: "jobs.gpu", want: Attribution{"tasks", "jobs.gpu"}},
		{key: "flow.abc123", want: Attribution{"tasks", "flow.abc123"}},
		{key: "cap.image", want: Attribution{"tasks", "cap.image"}},
		// mailboxes: the .inbox suffix is the mailbox queue family
		{key: "agent.brain.inbox", want: Attribution{"tasks", "agent.brain.inbox"}},
		{key: "sonnet.inbox", want: Attribution{"tasks", "sonnet.inbox"}},
		// events
		{key: "job.d41.done", want: Attribution{"events", EventsQueue}},
		{key: "agent.d41.waiting", want: Attribution{"events", EventsQueue}},
		{key: "window.open", want: Attribution{"events", EventsQueue}},
		{key: "lease.acquired", want: Attribution{"events", EventsQueue}},
		{key: "stream.started", want: Attribution{"events", EventsQueue}},
		// cadence keys land on their cadence queue (bound <name>.#)
		{key: "sched.hourly.tick", want: Attribution{"events", "sched.hourly"}},
		{key: "sched.monthly.end", want: Attribution{"events", "sched.monthly"}},
	}
	for _, c := range cases {
		if got := attribute(c.key); got != c.want {
			t.Errorf("attribute(%q) = %+v, want %+v", c.key, got, c.want)
		}
	}
}

func TestMessageID(t *testing.T) {
	// 1. the AMQP id wins over everything
	if got := messageID("amqp-1", []byte(`{"id":"x"}`)); got != "amqp-1" {
		t.Errorf("amqp id: %q", got)
	}
	// 2-4. envelope fields, in order
	if got := messageID("", []byte(`{"id":"job-1","job_id":"j"}`)); got != "job-1" {
		t.Errorf("envelope id: %q", got)
	}
	if got := messageID("", []byte(`{"correlation_id":"cap-9"}`)); got != "cap-9" {
		t.Errorf("correlation_id: %q", got)
	}
	if got := messageID("", []byte(`{"job_id":"job-42"}`)); got != "job-42" {
		t.Errorf("job_id: %q", got)
	}
	// 5. content hash: stable per body, prefixed h
	a := messageID("", []byte(`{"no":"ids"}`))
	b := messageID("", []byte(`{"no":"ids"}`))
	c := messageID("", []byte(`{"no":"other"}`))
	if a != b || len(a) != 1+16 || a[0] != 'h' || a == c {
		t.Errorf("hash fallback: %q %q %q", a, b, c)
	}
}
