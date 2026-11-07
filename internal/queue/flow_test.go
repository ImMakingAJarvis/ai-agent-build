package queue

import "testing"

func TestFlowQueueName(t *testing.T) {
	if got := FlowQueueName("avatar"); got != "flow.avatar" {
		t.Errorf("FlowQueueName(avatar) = %q, want flow.avatar", got)
	}
}

func TestIsFlowQueue(t *testing.T) {
	for _, q := range []string{"flow.avatar", "flow.mq-e2e", FlowQueueName("x.y")} {
		if !IsFlowQueue(q) {
			t.Errorf("IsFlowQueue(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"voice", "fix", "coding", "background", "jobs.gpu", "jobs.cpu", "jobs.igpu", "flow", ""} {
		if IsFlowQueue(q) {
			t.Errorf("IsFlowQueue(%q) = true, want false", q)
		}
	}
}
