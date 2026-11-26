package loop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
)

// tickJobID and tickInstructions are the job every tick test ingests.
// The fake engine keys a conversation on the user message's content, so
// the instructions double as the script key.
const (
	tickJobID        = "j1"
	tickInstructions = "count to three"
)

// newTickEnv builds a loop over a real file store, the given engine and
// a runner for the given tools. The slot manager reads the gateJobs map
// (gate_test.go): no test job ever records a snapshot, so every lease
// is cold and the map stays empty.
func newTickEnv(t *testing.T, eng engine.Engine, tools ...ops.Tool) (*Loop, *agentjob.Store, *ops.Runner) {
	t.Helper()
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(agentjob.Spec{
		ID: tickJobID, Title: "tick job", Instructions: tickInstructions,
		Driver: "loop", Workspace: "/ws/" + tickJobID,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	reg := ops.NewRegistry(tools...)
	runner := &ops.Runner{Registry: reg}
	mgr := slot.New(slot.Config{Slots: cfg.Slots, SnapshotMinTokens: cfg.SnapshotMinTokens},
		eng, &gateJobs{states: map[string]slot.JobState{}, warm: map[string]int{}})
	l := New(Deps{
		Engine: eng, Slots: mgr, Runner: runner, Registry: reg,
		Store: store, Config: cfg, Now: time.Now,
	})
	return l, store, runner
}

// captureTool is a test tool: Run records the Call it received and
// returns res.
type captureTool struct {
	name  string
	res   ops.Result
	gotCh chan ops.Call
}

func (c captureTool) Spec() ops.Spec {
	return ops.Spec{Name: c.name, Description: "test tool", Class: ops.Quick}
}

func (c captureTool) Run(ctx context.Context, call ops.Call, progress func()) ops.Result {
	if c.gotCh != nil {
		c.gotCh <- call
	}
	return c.res
}

// TestTickFinalAnswer: a reply with no tool call ends the job — done,
// with the reply as its summary — and the slot goes back to the pool.
func TestTickFinalAnswer(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell"})
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
	if j.Tokens != 110 {
		t.Errorf("job tokens = %d, want 110", j.Tokens)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != "assistant" {
		t.Fatalf("messages = %+v, want one assistant message", msgs)
	}
	var a assistantContent
	if err := json.Unmarshal(msgs[0].Content, &a); err != nil {
		t.Fatalf("message content %s: %v", msgs[0].Content, err)
	}
	if a.Content != "all done" || len(a.ToolCalls) != 0 {
		t.Errorf("message content = %+v, want the plain answer", a)
	}
	if msgs[0].Tokens != 110 {
		t.Errorf("message tokens = %d, want 110", msgs[0].Tokens)
	}
	if ops, err := store.Ops(tickJobID); err != nil || len(ops) != 0 {
		t.Errorf("ops = %d (err %v), want none", len(ops), err)
	}
	// The slot went back: another job can take it.
	if _, err := l.deps.Slots.Acquire(context.Background(), "other"); err != nil {
		t.Errorf("slot not released: Acquire(other) = %v", err)
	}
}

// TestTickToolCall: a reply with a tool call leaves the job waiting, its
// op running with a started_at, and the runner holds the call.
func TestTickToolCall(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{
		name:  "shell",
		res:   ops.Result{Status: agentjob.OpDone, Output: "ok"},
		gotCh: got,
	})
	eng.Script(tickInstructions, fake.Step{
		Reply: engine.Message{
			Role: "assistant",
			ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{"cmd":"ls"}`)},
			},
		},
		PromptTokens: 100, OutputTokens: 20,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Waiting || j.WaitReason != agentjob.WaitOperation {
		t.Errorf("job is %q (wait_reason %q), want waiting (%s)",
			j.Status, j.WaitReason, agentjob.WaitOperation)
	}
	ops, err := store.Ops(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(ops))
	}
	op := ops[0]
	if op.ID != "call_1" || op.ToolCallID != "call_1" || op.Tool != "shell" || op.Status != agentjob.OpRunning {
		t.Errorf("op = %+v, want call_1/shell/running", op)
	}
	if op.StartedAt == nil {
		t.Error("op has no started_at")
	}
	// The runner received the call: the job's workspace, the op's log
	// path and the model's arguments travel with it.
	select {
	case c := <-got:
		if c.JobID != tickJobID || c.OpID != "call_1" || c.Name != "shell" {
			t.Errorf("call = %+v, want %s/call_1/shell", c, tickJobID)
		}
		if c.Workspace != "/ws/"+tickJobID {
			t.Errorf("workspace = %q, want %q", c.Workspace, "/ws/"+tickJobID)
		}
		if c.LogPath != store.OpLogPath(tickJobID, "call_1") {
			t.Errorf("log path = %q, want %q", c.LogPath, store.OpLogPath(tickJobID, "call_1"))
		}
		var args map[string]string
		if err := json.Unmarshal(c.Args, &args); err != nil || args["cmd"] != "ls" {
			t.Errorf("args = %s (err %v), want cmd ls", c.Args, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tool never received the call")
	}
}

// TestTickUnknownTool: a call for a tool the registry does not hold is
// not started; its op is checkpointed and its failure is reported
// through HandleOpDone. (Until m3-4c that handler is the stub, so the
// tick's error here is the stub's — the assertions below hold either
// way.)
func TestTickUnknownTool(t *testing.T) {
	eng := fake.New(1)
	l, store, runner := newTickEnv(t, eng) // no tools at all
	eng.Script(tickInstructions, fake.Step{
		Reply: engine.Message{
			Role: "assistant",
			ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "nope", Arguments: json.RawMessage(`{}`)},
			},
		},
		PromptTokens: 100, OutputTokens: 20,
	})
	_ = l.tick(context.Background(), tickJobID)

	if running := runner.Running(); len(running) != 0 {
		t.Errorf("runner is running %v, want nothing (unknown tool is not started)", running)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status == agentjob.Running {
		t.Errorf("job is still running after the tick")
	}
	ops, err := store.Ops(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Tool != "nope" || ops[0].ToolCallID != "call_1" {
		t.Errorf("ops = %+v, want one op for the unknown call", ops)
	}
}

// TestTickEngineErrorPutsJobBack: a failed model round gives the slot
// back with the pre-tick token count, writes no checkpoint, and puts
// the job back to ready.
func TestTickEngineErrorPutsJobBack(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng)
	boom := errors.New("engine down")
	eng.Script(tickInstructions, fake.Step{Err: boom})
	err := l.tick(context.Background(), tickJobID)
	if !errors.Is(err, boom) {
		t.Fatalf("tick err = %v, want the engine error", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("status = %q, want ready", j.Status)
	}
	if j.Tokens != 0 {
		t.Errorf("tokens = %d, want 0 (the pre-tick count)", j.Tokens)
	}
	if msgs, err := store.Messages(tickJobID); err != nil || len(msgs) != 0 {
		t.Errorf("messages = %d (err %v), want none (no checkpoint)", len(msgs), err)
	}
	// The slot went back: another job can take it.
	if _, err := l.deps.Slots.Acquire(context.Background(), "other"); err != nil {
		t.Errorf("slot not released: Acquire(other) = %v", err)
	}
}

// recEngine records the ChatRequests it forwards to the fake engine.
type recEngine struct {
	*fake.Engine
	mu   sync.Mutex
	reqs []engine.ChatRequest
}

func (r *recEngine) Chat(ctx context.Context, req engine.ChatRequest) (engine.Stream, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.Engine.Chat(ctx, req)
}

func (r *recEngine) requests() []engine.ChatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]engine.ChatRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// TestPromptStablePrefix: two ticks of the same job send the SAME first
// two messages — the engine's prompt cache depends on the prefix being
// byte-identical — and the second request carries the first tick's
// assistant message.
func TestPromptStablePrefix(t *testing.T) {
	eng := fake.New(1)
	rec := &recEngine{Engine: eng}
	l, store, _ := newTickEnv(t, rec, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone}})
	eng.Script(tickInstructions,
		fake.Step{
			Reply: engine.Message{
				Role: "assistant",
				ToolCalls: []engine.ToolCall{
					{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{}`)},
				},
			},
			PromptTokens: 100, OutputTokens: 20,
		},
		fake.Step{
			Reply:        engine.Message{Role: "assistant", Content: "done"},
			PromptTokens: 150, OutputTokens: 5,
		},
	)
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	reqs := rec.requests()
	if len(reqs) != 2 {
		t.Fatalf("chats = %d, want 2", len(reqs))
	}
	if !reflect.DeepEqual(reqs[0].Messages[:2], reqs[1].Messages[:2]) {
		t.Errorf("the first two messages differ between ticks:\nfirst:  %+v\nsecond: %+v",
			reqs[0].Messages[:2], reqs[1].Messages[:2])
	}
	if len(reqs[1].Messages) != 3 {
		t.Fatalf("second request has %d messages, want 3", len(reqs[1].Messages))
	}
	last := reqs[1].Messages[2]
	if last.Role != "assistant" || len(last.ToolCalls) != 1 || last.ToolCalls[0].ID != "call_1" {
		t.Errorf("second request's third message = %+v, want tick 1's assistant tool call", last)
	}
	if j, _ := store.Get(tickJobID); j.Status != agentjob.Done || j.Summary != "done" {
		t.Errorf("job is %q (summary %q), want done (summary %q)", j.Status, j.Summary, "done")
	}
}
