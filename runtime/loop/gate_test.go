package loop

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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

// TestGateA proves the whole point of the project (card m3-4a): while
// job A waits for a slow tool call, job B uses the engine slot. It is
// written BEFORE the loop exists — the Skip keeps it out of the gate
// until card m3-4c implements Submit, HandleOpDone and Step, then
// deletes the Skip and changes nothing else.
func TestGateA(t *testing.T) {
	t.Skip("enabled by card m3-4c")

	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for id, task := range map[string]string{"a": "task A", "b": "task B"} {
		if _, err := store.Ingest(agentjob.Spec{ID: id, Title: task, Instructions: task, Driver: "loop"}); err != nil {
			t.Fatal(err)
		}
	}

	eng := fake.New(1)
	eng.Script("task A",
		fake.Step{
			Reply: engine.Message{
				Role: "assistant",
				ToolCalls: []engine.ToolCall{
					{ID: "call_1", Name: "slow", Arguments: json.RawMessage(`{}`)},
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

	release := make(chan struct{})
	reg := ops.NewRegistry(slowTool{release: release})

	cfg := config.Default()
	mgr := slot.New(
		slot.Config{Slots: cfg.Slots, SnapshotMinTokens: cfg.SnapshotMinTokens},
		eng,
		&gateJobs{states: map[string]slot.JobState{}, warm: map[string]int{}},
	)

	l := New(Deps{
		Engine:   eng,
		Slots:    mgr,
		Registry: reg,
		Store:    store,
		Config:   cfg,
		Now:      time.Now,
	})
	l.deps.Runner = &ops.Runner{
		Registry: reg,
		OnDone: func(jobID, opID string, res ops.Result) {
			if err := l.HandleOpDone(jobID, opID, res); err != nil {
				t.Errorf("HandleOpDone(%s, %s): %v", jobID, opID, err)
			}
		},
	}

	ctx := context.Background()
	if err := l.Submit("a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("b"); err != nil {
		t.Fatal(err)
	}

	// A's first tick: the model calls the slow tool, the op starts in the
	// background, A gives the slot back and waits for it.
	ran, err := l.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("step 1: A could not run")
	}
	if j, _ := store.Get("a"); j.Status != agentjob.Waiting {
		t.Fatalf("after A's first tick: job a is %q, want waiting", j.Status)
	}

	// The slot was free while A's tool was running: B's tick runs and B
	// is done. This is the proof.
	ran, err = l.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("step 2: B could not run while A's tool was in flight")
	}
	if j, _ := store.Get("b"); j.Status != agentjob.Done {
		t.Fatalf("after B's tick: job b is %q, want done", j.Status)
	}

	// Release the slow tool, then wait for A's op to finish and A to come
	// back ready (HandleOpDone marks the op done and resubmits the job).
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := store.Get("a")
		if err == nil && j.Status == agentjob.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job a did not come back to ready (status %q, err %v)", j.Status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A's second tick: the model finishes, A is done.
	ran, err = l.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("step 3: A could not finish")
	}

	// The model was asked A, then B, then A again: B got the slot while
	// A's tool was running.
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

	msgs, err := store.Messages("a")
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("job a has %d messages %v, want %v", len(roles), roles, wantRoles)
	}
	for i := range wantRoles {
		if roles[i] != wantRoles[i] {
			t.Errorf("job a message %d is %q, want %q (all: %v)", i, roles[i], wantRoles[i], roles)
		}
	}

	if j, _ := store.Get("a"); j.Status != agentjob.Done {
		t.Errorf("job a is %q, want done", j.Status)
	}
	if j, _ := store.Get("b"); j.Status != agentjob.Done {
		t.Errorf("job b is %q, want done", j.Status)
	}
	if j, _ := store.Get("b"); !strings.Contains(j.Summary, "B done") {
		t.Errorf("job b summary %q does not contain %q", j.Summary, "B done")
	}
}

// slowTool is the "slow" tool: Run blocks until the test closes release,
// then reports done.
type slowTool struct{ release chan struct{} }

// Spec declares the tool to the registry and the model.
func (s slowTool) Spec() ops.Spec {
	return ops.Spec{Name: "slow", Description: "blocks until released", Class: ops.Long}
}

// Run blocks until release is closed (or the op is cancelled), then
// returns Status done, Output "ok".
func (s slowTool) Run(ctx context.Context, c ops.Call, progress func()) ops.Result {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ops.Result{Status: agentjob.OpCancelled, Error: ctx.Err().Error()}
	}
	return ops.Result{Status: agentjob.OpDone, Output: "ok"}
}

// gateJobs is the slot.Jobs the slot manager reads and writes in the test:
// a map. No cache in the test ever reaches SnapshotMinTokens, so no
// snapshot is saved and the maps never need to outlive the process.
type gateJobs struct {
	mu     sync.Mutex
	states map[string]slot.JobState
	warm   map[string]int
}

// Get returns the job's recorded slot state; an unknown job is not found.
func (j *gateJobs) Get(id string) (slot.JobState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	js, ok := j.states[id]
	if !ok {
		return slot.JobState{}, os.ErrNotExist
	}
	return js, nil
}

// SetSnapshot records the snapshot the job's cache was saved to.
func (j *gateJobs) SetSnapshot(id, file, fingerprint string, tokens int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	js := j.states[id]
	js.ID = id
	js.Snapshot = file
	js.SnapshotFingerprint = fingerprint
	js.Tokens = tokens
	j.states[id] = js
	return nil
}

// SetWarmSlot records the slot the job's cache is warm in; nil clears it.
func (j *gateJobs) SetWarmSlot(id string, s *int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s == nil {
		delete(j.warm, id)
	} else {
		j.warm[id] = *s
	}
	return nil
}
