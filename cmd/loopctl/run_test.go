package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
)

// TestRunTwoJobs runs the run command end to end with the fake engine
// scripted like Gate A (runtime/loop/gate_test.go): job A's first reply
// calls a tool and job B finishes in the slot while A waits. Here the
// tool is the real shell tool running `sleep 1`, so the wait is real.
// The exit code must be 0 and the report must carry both ids and the
// idle line.
func TestRunTwoJobs(t *testing.T) {
	// A throwaway store and a missing config file (= the defaults), so the
	// test never touches the box's real queue or config.
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	t.Setenv("LOOPD_CONFIG", filepath.Join(t.TempDir(), "loopd.yaml"))
	ws := t.TempDir()

	eng := fake.New(1)
	eng.Script("task A",
		fake.Step{
			Reply: engine.Message{
				Role: "assistant",
				ToolCalls: []engine.ToolCall{
					{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{"cmd": "sleep 1"}`)},
				},
			},
			PromptTokens: 100,
			OutputTokens: 50,
		},
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "A done"},
			PromptTokens: 150,
			OutputTokens: 10,
		},
	)
	eng.Script("task B",
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "B done"},
			PromptTokens: 100,
			OutputTokens: 10,
		},
	)

	var out strings.Builder
	code := run([]string{"run", "--workspace", ws, "task A", "task B"}, eng, &out)
	if code != 0 {
		t.Fatalf("run exited %d, want 0:\n%s", code, out.String())
	}
	s := out.String()

	// The report names both jobs, done.
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`loopctl-\d+-1\s+status=done\s+ticks=2`),
		regexp.MustCompile(`loopctl-\d+-2\s+status=done\s+ticks=1`),
	} {
		if !re.MatchString(s) {
			t.Errorf("report missing %q:\n%s", re, s)
		}
	}
	if !strings.Contains(s, "idle_while_ready_seconds:") {
		t.Errorf("report missing the idle line:\n%s", s)
	}

	// The Gate A point: the model was asked A, then B (while A's sleep ran
	// in the shell tool's goroutine), then A again to finish.
	uses := eng.Timeline()
	want := []string{"task A", "task B", "task A"}
	if len(uses) != len(want) {
		t.Fatalf("timeline has %d chats, want %d: %+v", len(uses), len(want), uses)
	}
	for i := range want {
		if uses[i].Key != want[i] {
			t.Errorf("timeline[%d] = %q, want %q", i, uses[i].Key, want[i])
		}
	}
}
