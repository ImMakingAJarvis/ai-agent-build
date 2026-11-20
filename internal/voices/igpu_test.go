package voices

import (
	"strings"
	"testing"
)

// The brain must be able to tell the owner how to queue work on the iGPU
// worker (2026-11-19: asked on Discord, it did not know !igpu existed).
func TestBuiltinVoicesKnowIGPU(t *testing.T) {
	for _, key := range []string{"jarvis", "glados"} {
		v := Get(key)
		if v == nil {
			t.Fatalf("voice %q not registered", key)
		}
		if !strings.Contains(v.SystemPrompt, IGPUGuide) || !strings.Contains(v.SystemPrompt, "!igpu <task>") {
			t.Errorf("voice %q prompt does not explain !igpu", key)
		}
	}
}
