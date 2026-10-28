package queue

import "testing"

func TestQueueDeclareArgsMatchBroker(t *testing.T) {
	want := map[string]int32{
		VoiceQueue:      10,
		CodingQueue:     5,
		BackgroundQueue: 1,
	}
	for name, pri := range want {
		args := queueDeclareArgs(name)
		got, ok := args["x-priority"].(int32)
		if !ok || got != pri {
			t.Errorf("%s x-priority = %v (%T), want %d", name, args["x-priority"], args["x-priority"], pri)
		}
	}
	if args := queueDeclareArgs(FixQueue); args != nil {
		t.Errorf("fix queue args = %v, want nil (not in definitions.json)", args)
	}
}
