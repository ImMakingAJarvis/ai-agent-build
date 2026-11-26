// loop_test.go covers the card m3-4c additions to the loop (card
// ELR-31): repeated op events are no-ops, a job with several tool calls
// comes back ready only when the last one finishes, the budgets fail a
// job instead of ticking it, a step with no free slot leaves the ready
// list untouched, and a big result is spilled to a file.
package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// oneCallScript is a first tick with one tool call to the named tool.
func oneCallScript(callID, tool string) fake.Step {
	return fake.Step{
		Reply: engine.Message{
			Role: "assistant",
			ToolCalls: []engine.ToolCall{
				{ID: callID, Name: tool, Arguments: json.RawMessage(`{}`)},
			},
		},
		PromptTokens: 100, OutputTokens: 20,
	}
}

// TestHandleOpDoneTwiceIsNoop: a repeated op event changes nothing —
// no second tool message, no state change.
func TestHandleOpDoneTwiceIsNoop(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}})
	eng.Script(tickInstructions, oneCallScript("call_1", "shell"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	res := ops.Result{Status: agentjob.OpDone, Output: "ok"}
	if err := l.HandleOpDone(tickJobID, "call_1", res); err != nil {
		t.Fatalf("first HandleOpDone: %v", err)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Get(tickJobID)

	if err := l.HandleOpDone(tickJobID, "call_1", res); err != nil {
		t.Fatalf("second HandleOpDone: %v", err)
	}
	msgs2, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != len(msgs) {
		t.Errorf("messages = %d after the repeat, want %d (a repeated event adds nothing)", len(msgs2), len(msgs))
	}
	second, _ := store.Get(tickJobID)
	if second.Status != first.Status {
		t.Errorf("status = %q after the repeat, want %q", second.Status, first.Status)
	}
	var toolMsgs int
	for _, m := range msgs2 {
		if m.Role == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs != 1 {
		t.Errorf("tool messages = %d, want 1", toolMsgs)
	}
}

// TestTwoToolCallsWaitForBoth: a tick with two tool calls leaves the job
// waiting until the LAST result arrives; only then is it ready again and
// back in the ready list.
func TestTwoToolCallsWaitForBoth(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng,
		captureTool{name: "shell"}, captureTool{name: "grep"})
	eng.Script(tickInstructions, fake.Step{
		Reply: engine.Message{
			Role: "assistant",
			ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{}`)},
				{ID: "call_2", Name: "grep", Arguments: json.RawMessage(`{}`)},
			},
		},
		PromptTokens: 100, OutputTokens: 20,
	})
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}

	// The first result: the other op is still running, the job stays
	// waiting and is not submitted.
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "a"}); err != nil {
		t.Fatal(err)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Waiting {
		t.Fatalf("after the first result the job is %q, want waiting", j.Status)
	}
	l.mu.Lock()
	n := len(l.ready)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("ready list has %d jobs after the first result, want 0", n)
	}

	// The second result: every op is done — ready, and submitted.
	if err := l.HandleOpDone(tickJobID, "call_2", ops.Result{Status: agentjob.OpDone, Output: "b"}); err != nil {
		t.Fatal(err)
	}
	j, _ := store.Get(tickJobID)
	if j.Status != agentjob.Ready || j.WaitReason != "" {
		t.Errorf("job is %q (wait_reason %q), want ready with no wait reason", j.Status, j.WaitReason)
	}
	l.mu.Lock()
	ready := append([]string(nil), l.ready...)
	l.mu.Unlock()
	if len(ready) != 1 || ready[0] != tickJobID {
		t.Errorf("ready list = %v, want [%s]", ready, tickJobID)
	}
}

// TestBudgetFailsJob: a job at or over the token budget is failed with
// the budget error and removed from the ready list; the model is not
// asked again.
func TestBudgetFailsJob(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}})
	l.deps.Config.MaxTaskTokens = 100
	eng.Script(tickInstructions,
		oneCallScript("call_1", "shell"), // leaves the job at 120 tokens
		fake.Step{Reply: engine.Message{Role: "assistant", Content: "late"}, PromptTokens: 100, OutputTokens: 10},
	)
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ran, err := l.Step(ctx); err != nil || !ran {
		t.Fatalf("step 1 = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}

	// The token count is over the budget: the job is failed, not ticked.
	ran, err := l.Step(ctx)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if !ran {
		t.Error("step 2: the over-budget job was not processed")
	}
	j, _ := store.Get(tickJobID)
	if j.Status != agentjob.Failed {
		t.Errorf("job is %q, want failed", j.Status)
	}
	if j.Error != "budget: tick or token limit reached" {
		t.Errorf("error = %q, want the budget error", j.Error)
	}
	l.mu.Lock()
	n := len(l.ready)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("ready list has %d jobs, want 0", n)
	}
	if uses := eng.Timeline(); len(uses) != 1 {
		t.Errorf("timeline has %d chats, want 1 (the budget check runs before the model)", len(uses))
	}
}

// TestStepNoSlot: with every slot taken, Step runs nothing and leaves
// the job first in the ready list.
func TestStepNoSlot(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng)
	eng.Script(tickInstructions, fake.Step{
		Reply: engine.Message{Role: "assistant", Content: "x"},
		PromptTokens: 10, OutputTokens: 10,
	})
	lease, err := l.deps.Slots.Acquire(context.Background(), "busy")
	if err != nil {
		t.Fatalf("holding the only slot: %v", err)
	}
	defer l.deps.Slots.Release(lease, 0)

	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	ran, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if ran {
		t.Error("step ran with no slot free")
	}
	l.mu.Lock()
	var first string
	n := len(l.ready)
	if n > 0 {
		first = l.ready[0]
	}
	l.mu.Unlock()
	if n != 1 || first != tickJobID {
		t.Errorf("ready list = %d jobs (first %q), want [%s]", n, first, tickJobID)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Ready {
		t.Errorf("job is %q, want ready", j.Status)
	}
	if uses := eng.Timeline(); len(uses) != 0 {
		t.Errorf("timeline has %d chats, want 0", len(uses))
	}
}

// TestBigOutputIsSpilled: a 100 KB result never enters the tool message
// whole — the message stays under 12 KB and the full text is in the
// .full file.
func TestBigOutputIsSpilled(t *testing.T) {
	lines := make([]string, 0, 5000)
	for i := 0; i < 5000; i++ {
		lines = append(lines, "line of output " + strings.Repeat("x", 10))
	}
	big := strings.Join(lines, "\n")
	if len(big) < 100*1024 {
		t.Fatalf("test output is %d bytes, want at least 100 KB", len(big))
	}

	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell"})
	eng.Script(tickInstructions, oneCallScript("call_1", "shell"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: big}); err != nil {
		t.Fatal(err)
	}

	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	var tc toolContent
	found := false
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "tool" {
			continue
		}
		if err := json.Unmarshal(msgs[i].Content, &tc); err != nil {
			t.Fatalf("tool message content %s: %v", msgs[i].Content, err)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no tool message in the log")
	}
	if tc.ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q, want call_1", tc.ToolCallID)
	}
	if len(tc.Content) >= 12*1024 {
		t.Errorf("the tool message is %d bytes, want under 12 KB", len(tc.Content))
	}

	full, err := os.ReadFile(filepath.Join(store.JobDir(tickJobID), "ops", "call_1.full"))
	if err != nil {
		t.Fatalf("the .full file is missing: %v", err)
	}
	if string(full) != big {
		t.Errorf("the .full file is %d bytes, want the full %d-byte output", len(full), len(big))
	}
}
