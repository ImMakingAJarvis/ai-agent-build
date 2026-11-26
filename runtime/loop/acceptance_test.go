// acceptance_test.go covers the acceptance gate (card ELR-89, M3.4e): a
// job with acceptance commands in its spec is done only when those
// commands pass, and the commands run as the runtime's own internal
// shell ops — never the model's. The env here is like newTickEnv except
// the workspace is a real temp dir and the runner reports results
// through HandleOpDone, so the shell and write_file tools run for real.
package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/tools"
)

// newAcceptanceEnv builds a loop over a real file store with the real
// shell and write_file tools confined to a temp workspace. The runner
// reports results through HandleOpDone, the way loopd wires it.
func newAcceptanceEnv(t *testing.T, eng engine.Engine, acceptance []string) (*Loop, *agentjob.Store, string) {
	t.Helper()
	ws := t.TempDir()
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(agentjob.Spec{
		ID: tickJobID, Title: "acceptance job", Instructions: tickInstructions,
		Driver: "loop", Workspace: ws, Acceptance: acceptance,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	g := &perm.Guard{WorkDir: ws}
	reg := ops.NewRegistry(tools.Shell{Guard: g}, tools.WriteFile{Guard: g})
	var l *Loop
	runner := &ops.Runner{
		Registry: reg,
		OnDone: func(jobID, opID string, res ops.Result) {
			if err := l.HandleOpDone(jobID, opID, res); err != nil {
				t.Errorf("HandleOpDone(%s, %s): %v", jobID, opID, err)
			}
		},
	}
	mgr := slot.New(slot.Config{Slots: cfg.Slots, SnapshotMinTokens: cfg.SnapshotMinTokens},
		eng, &gateJobs{states: map[string]slot.JobState{}, warm: map[string]int{}})
	l = New(Deps{
		Engine: eng, Slots: mgr, Runner: runner, Registry: reg,
		Store: store, Config: cfg, Now: time.Now,
	})
	return l, store, ws
}

// waitStatus polls until the job's status is want and returns it: the
// acceptance op runs in the Runner's goroutine, outside the tick, so the
// status settles asynchronously.
func waitStatus(t *testing.T, store *agentjob.Store, jobID, want string) agentjob.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := store.Get(jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached %s (status %q)", jobID, want, j.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAcceptancePassMakesDone: the model writes the file the acceptance
// command checks and then says done; the runtime runs the command itself
// as an internal op and makes the job done with the model's text as its
// summary (card ELR-89).
func TestAcceptancePassMakesDone(t *testing.T) {
	eng := fake.New(1)
	l, store, ws := newAcceptanceEnv(t, eng, []string{"test -f ok.txt"})
	eng.Script(tickInstructions,
		fake.Step{
			Reply: engine.Message{Role: "assistant", ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "write_file", Arguments: json.RawMessage(`{"path":"ok.txt","content":"ok"}`)},
			}},
			PromptTokens: 100, OutputTokens: 10,
		},
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "done"},
			PromptTokens: 100, OutputTokens: 5,
		},
	)
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// The op runs in the Runner's goroutine: ready means write_file
	// finished and its result is in the log.
	waitStatus(t, store, tickJobID, agentjob.Ready)
	if _, err := os.Stat(filepath.Join(ws, "ok.txt")); err != nil {
		t.Fatalf("ok.txt was not written by the real tool: %v", err)
	}
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	// The model's "done" did not finish the job: the runtime ran the
	// acceptance command, it passed, and the job is done.
	j := waitStatus(t, store, tickJobID, agentjob.Done)
	if j.Summary != "done" {
		t.Errorf("summary = %q, want the model's last text %q", j.Summary, "done")
	}
	op, err := store.GetOp(tickJobID, "accept-1")
	if err != nil {
		t.Fatal(err)
	}
	if op.ToolCallID != "" || op.Tool != "shell" || op.Status != agentjob.OpDone {
		t.Errorf("accept-1 = %+v, want an internal shell op, done", op)
	}
	var args map[string]string
	if err := json.Unmarshal(op.Args, &args); err != nil || args["cmd"] != "test -f ok.txt" {
		t.Errorf("accept-1 args = %s (err %v), want the job's acceptance command", op.Args, err)
	}
}

// TestAcceptanceFailGoesBackToModel: the model says done without having
// created the file; the failed acceptance check comes back to the model
// as a user message and the job is ready again — and the fixed work
// passes on the next done.
func TestAcceptanceFailGoesBackToModel(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newAcceptanceEnv(t, eng, []string{"test -f ok.txt"})
	eng.Script(tickInstructions,
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "done"},
			PromptTokens: 100, OutputTokens: 5,
		},
		fake.Step{
			Reply: engine.Message{Role: "assistant", ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "write_file", Arguments: json.RawMessage(`{"path":"ok.txt","content":"ok"}`)},
			}},
			PromptTokens: 100, OutputTokens: 10,
		},
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "done for real"},
			PromptTokens: 100, OutputTokens: 5,
		},
	)
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// The check failed: the job went back to ready, not done.
	waitStatus(t, store, tickJobID, agentjob.Ready)
	prompt, err := l.prompt(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range prompt {
		if strings.Contains(m.Content, "acceptance check failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("the next prompt does not carry the acceptance failure:\n%+v", prompt)
	}
	// The model fixes the cause, says done again, and this time the
	// check passes.
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	waitStatus(t, store, tickJobID, agentjob.Ready) // write_file finished
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	j := waitStatus(t, store, tickJobID, agentjob.Done)
	if j.Summary != "done for real" {
		t.Errorf("summary = %q, want %q", j.Summary, "done for real")
	}
}

// TestAcceptanceFailLimit: a job whose acceptance check keeps failing is
// failed after max_acceptance_fails (5 by default) failed checks, with
// the count in its error.
func TestAcceptanceFailLimit(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newAcceptanceEnv(t, eng, []string{"test -f ok.txt"})
	var steps []fake.Step
	for i := 0; i < l.deps.Config.MaxAcceptanceFails; i++ {
		steps = append(steps, fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "done"},
			PromptTokens: 100, OutputTokens: 5,
		})
	}
	eng.Script(tickInstructions, steps...)
	for i := 1; i < l.deps.Config.MaxAcceptanceFails; i++ {
		if err := l.tick(context.Background(), tickJobID); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		waitStatus(t, store, tickJobID, agentjob.Ready)
	}
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, store, tickJobID, agentjob.Failed)
	if j.Error != "acceptance check failed 5 times" {
		t.Errorf("error = %q, want %q", j.Error, "acceptance check failed 5 times")
	}
}

// TestNoAcceptanceUnchanged: a job without acceptance commands behaves
// exactly as before — a plain final answer makes it done on the spot,
// no ops at all.
func TestNoAcceptanceUnchanged(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newAcceptanceEnv(t, eng, nil)
	eng.Script(tickInstructions, fake.Step{
		Reply:        engine.Message{Role: "assistant", Content: "all done"},
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Done || j.Summary != "all done" {
		t.Errorf("job is %q (summary %q), want done (summary %q)", j.Status, j.Summary, "all done")
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 0 {
		t.Errorf("ops = %d (err %v), want none", len(opsList), err)
	}
}

// TestAcceptanceOpSurvivesRecover: a crash between the checkpoint and
// the op's start leaves a waiting job with a pending accept op; Recover
// starts the op again and the job settles when it finishes (card
// ELR-89).
func TestAcceptanceOpSurvivesRecover(t *testing.T) {
	eng := fake.New(1)
	l, store, ws := newAcceptanceEnv(t, eng, []string{"test -f ok.txt"})
	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The checkpoint a crashed tick would have left: the model's done,
	// the pending accept op, the waiting state.
	content, err := json.Marshal(assistantContent{Content: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessages(tickJobID, agentjob.LoopMessage{
		Role: "assistant", Content: content, Tokens: 110,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp(tickJobID, agentjob.LoopOp{
		ID:     "accept-1",
		Tool:   "shell",
		Args:   json.RawMessage(`{"cmd":"test -f ok.txt"}`),
		Status: agentjob.OpPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus(tickJobID, agentjob.State{
		Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Recover([]string{tickJobID}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// The pending accept op ran for real, passed, and settled the job.
	j := waitStatus(t, store, tickJobID, agentjob.Done)
	if j.Summary != "done" {
		t.Errorf("summary = %q, want %q", j.Summary, "done")
	}
	op, err := store.GetOp(tickJobID, "accept-1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != agentjob.OpDone {
		t.Errorf("accept-1 status = %q, want %s", op.Status, agentjob.OpDone)
	}
}
