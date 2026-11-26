// afteredit_test.go covers card ELR-37 (M3.7b): a tick that ends with a
// file edit runs the config's after_edit commands once, as one internal
// shell op with no tool call, and the result reaches the model as a
// plain user message.
package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// checkOps returns the job's after_edit check ops: the ops with no tool
// call.
func checkOps(t *testing.T, store *agentjob.Store, jobID string) []agentjob.LoopOp {
	t.Helper()
	all, err := store.Ops(jobID)
	if err != nil {
		t.Fatalf("store.Ops: %v", err)
	}
	var out []agentjob.LoopOp
	for _, op := range all {
		if op.ToolCallID == "" {
			out = append(out, op)
		}
	}
	return out
}

// lastUserContent returns the content of the job's last user message.
func lastUserContent(t *testing.T, store *agentjob.Store, jobID string) string {
	t.Helper()
	msgs, err := store.Messages(jobID)
	if err != nil {
		t.Fatalf("store.Messages: %v", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(msgs[i].Content, &s); err != nil {
			t.Fatalf("user message %q: %v", msgs[i].Content, err)
		}
		return s
	}
	t.Fatal("no user message in the log")
	return ""
}

// TestAfterEditRunsOncePerTick: two edit_file calls in one tick create
// exactly ONE check op, when the last one finishes; the job stays
// waiting while the check runs.
func TestAfterEditRunsOncePerTick(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "edit_file"}, captureTool{name: "shell"})
	l.deps.Config.AfterEdit = []string{"go build ./..."}
	eng.Script(tickInstructions, fake.Step{
		Reply: engine.Message{
			Role: "assistant",
			ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "edit_file", Arguments: json.RawMessage(`{}`)},
				{ID: "call_2", Name: "edit_file", Arguments: json.RawMessage(`{}`)},
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

	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if n := len(checkOps(t, store, tickJobID)); n != 0 {
		t.Fatalf("%d check ops after the first edit, want 0 (the second op still runs)", n)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Waiting {
		t.Fatalf("job is %q after the first edit, want waiting", j.Status)
	}

	if err := l.HandleOpDone(tickJobID, "call_2", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	checks := checkOps(t, store, tickJobID)
	if len(checks) != 1 {
		t.Fatalf("check ops = %d, want exactly 1 for two edits in one tick", len(checks))
	}
	if checks[0].ID != "after-edit-1" || checks[0].Tool != "shell" {
		t.Errorf("check op = %s/%s, want after-edit-1/shell", checks[0].ID, checks[0].Tool)
	}
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(checks[0].Args, &args); err != nil {
		t.Fatalf("check op args: %v", err)
	}
	if args.Cmd != "go build ./..." {
		t.Errorf("check op cmd = %q, want the configured command", args.Cmd)
	}
	// While the check op runs the job is not ready: the next tick must
	// wait for the result.
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Waiting {
		t.Errorf("job is %q while the check op runs, want waiting", j.Status)
	}
	l.mu.Lock()
	n := len(l.ready)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("ready list has %d jobs while the check op runs, want 0", n)
	}
}

// TestAfterEditFailureReachesModel: a failing check (exit 3, "boom")
// reaches the model on the next prompt as a user message with the exit
// code and the output.
func TestAfterEditFailureReachesModel(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "edit_file"}, captureTool{name: "shell"})
	l.deps.Config.AfterEdit = []string{"echo boom; exit 3"}
	eng.Script(tickInstructions, oneCallScript("call_1", "edit_file"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if n := len(checkOps(t, store, tickJobID)); n != 1 {
		t.Fatalf("check ops = %d, want 1", n)
	}

	if err := l.HandleOpDone(tickJobID, "after-edit-1", ops.Result{Status: agentjob.OpFailed, ExitCode: 3, Output: "boom\n"}); err != nil {
		t.Fatal(err)
	}
	want := "[automatic check after your edits] exit code 3\nboom\n"
	if got := lastUserContent(t, store, tickJobID); got != want {
		t.Errorf("user message = %q, want %q", got, want)
	}

	// The next prompt carries the failure.
	prompt, err := l.prompt(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range prompt {
		if m.Role == "user" && strings.Contains(m.Content, "exit code 3") && strings.Contains(m.Content, "boom") {
			found = true
		}
	}
	if !found {
		t.Error("the next prompt has no user message carrying the check failure")
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Ready {
		t.Errorf("job is %q after the check failure, want ready", j.Status)
	}
}

// TestAfterEditPassIsShort: a passing check (exit 0) produces the one
// short user message, nothing more.
func TestAfterEditPassIsShort(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "edit_file"}, captureTool{name: "shell"})
	l.deps.Config.AfterEdit = []string{"go build ./..."}
	eng.Script(tickInstructions, oneCallScript("call_1", "edit_file"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := l.HandleOpDone(tickJobID, "after-edit-1", ops.Result{Status: agentjob.OpDone, Output: ""}); err != nil {
		t.Fatal(err)
	}
	want := "[automatic check after your edits] passed"
	if got := lastUserContent(t, store, tickJobID); got != want {
		t.Errorf("user message = %q, want %q", got, want)
	}
	prompt, err := l.prompt(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	var passes int
	for _, m := range prompt {
		if m.Role == "user" && m.Content == want {
			passes++
		}
	}
	if passes != 1 {
		t.Errorf("the next prompt has %d pass messages, want 1", passes)
	}
}

// TestNoAfterEditWithoutEdits: a tick with only non-edit ops never
// creates a check op, even when after_edit is configured.
func TestNoAfterEditWithoutEdits(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell"})
	l.deps.Config.AfterEdit = []string{"go build ./..."}
	eng.Script(tickInstructions, oneCallScript("call_1", "shell"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if n := len(checkOps(t, store, tickJobID)); n != 0 {
		t.Fatalf("check ops = %d, want 0 (no edit op finished)", n)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Ready {
		t.Errorf("job is %q, want ready without a check", j.Status)
	}
}

// TestNoAfterEditWhenNotConfigured: with no after_edit commands the
// edit op finishes like any other: ready, no check op.
func TestNoAfterEditWhenNotConfigured(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "edit_file"})
	eng.Script(tickInstructions, oneCallScript("call_1", "edit_file"))
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if err := l.HandleOpDone(tickJobID, "call_1", ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if n := len(checkOps(t, store, tickJobID)); n != 0 {
		t.Fatalf("check ops = %d, want 0 (after_edit not configured)", n)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Ready {
		t.Errorf("job is %q, want ready", j.Status)
	}
}
