package queue

import (
	"encoding/json"
	"os"
	"testing"
)

// TestReviewQueuesInDefinitions fails if any of the four review queue names
// (Job 3 of docs/pr-review-build-plan.md) is missing from
// rabbitmq/definitions.json. It parses the file — it does not shell out to
// a broker.
func TestReviewQueuesInDefinitions(t *testing.T) {
	data, err := os.ReadFile("../../rabbitmq/definitions.json")
	if err != nil {
		t.Fatalf("read rabbitmq/definitions.json: %v", err)
	}
	var defs struct {
		Queues []struct {
			Name string `json:"name"`
		} `json:"queues"`
	}
	if err := json.Unmarshal(data, &defs); err != nil {
		t.Fatalf("parse rabbitmq/definitions.json: %v", err)
	}
	names := make(map[string]bool, len(defs.Queues))
	for _, q := range defs.Queues {
		names[q.Name] = true
	}
	for _, want := range ReviewQueues {
		if !names[want] {
			t.Errorf("review queue %q is missing from rabbitmq/definitions.json", want)
		}
	}
}
