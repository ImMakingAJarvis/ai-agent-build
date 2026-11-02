package queue

import "testing"

func TestQueueDeclareArgsMatchBroker(t *testing.T) {
	want := map[string]int64{
		VoiceQueue:      10,
		CodingQueue:     5,
		BackgroundQueue: 10,
	}
	for name, pri := range want {
		args := queueDeclareArgs(name)
		got, ok := args["x-max-priority"].(int64)
		if !ok || got != pri {
			t.Errorf("%s x-max-priority = %v (%T), want %d", name, args["x-max-priority"], args["x-max-priority"], pri)
		}
	}
	if args := queueDeclareArgs(FixQueue); args != nil {
		t.Errorf("fix queue args = %v, want nil (not in definitions.json)", args)
	}
}
