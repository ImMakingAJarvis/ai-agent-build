package queue

import "testing"

func TestCanonicalEffort(t *testing.T) {
	cases := []struct {
		raw, kind, want string
	}{
		{"", "coding", EffortMedium},
		{"", "voice", EffortOff},
		{"unknown", "coding", EffortMedium},
		{"unknown", "voice", EffortOff},
		{"auto", "coding", EffortMedium},
		{"off", "coding", EffortOff},
		{"false", "coding", EffortOff},
		{"low", "coding", EffortLow},
		{"medium", "coding", EffortMedium},
		{"high", "coding", EffortXHigh},
		{"max", "coding", EffortXHigh},
		{"xhigh", "coding", EffortXHigh},
		{"HIGH", "coding", EffortXHigh},
	}
	for _, c := range cases {
		if got := CanonicalEffort(c.raw, c.kind); got != c.want {
			t.Errorf("CanonicalEffort(%q, %q)=%q want %q", c.raw, c.kind, got, c.want)
		}
	}
}

func TestEngineAndClaudeMap(t *testing.T) {
	if ClaudeEffort(EffortOff) != "" {
		t.Fatal("off must not be sent as --effort")
	}
	if ClaudeEffort(EffortXHigh) != EffortXHigh {
		t.Fatal("xhigh")
	}
	if EngineThinking(EffortOff) {
		t.Fatal("off disables thinking")
	}
	if !EngineThinking(EffortMedium) {
		t.Fatal("medium thinks")
	}
	if EngineReasoningEffort(EffortOff) != "" {
		t.Fatal("off has no jinja effort")
	}
	if EngineReasoningEffort(EffortHigh) != EffortXHigh {
		t.Fatal("high → xhigh for jinja")
	}
}
