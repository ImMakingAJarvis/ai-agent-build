// Tests for the operation runner (ELR-1, M3.1a, card ELR-24), using
// small fake tools.
package ops

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// doneEvent is one OnDone delivery tagged with its op id (TestTimeout
// starts several ops and must match the results to them).
type doneEvent struct {
	opID string
	res  Result
}

// fakeTool is a Tool whose behaviour the test scripts.
type fakeTool struct {
	spec Spec
	run  func(ctx context.Context, c Call, progress func()) Result
}

func (f *fakeTool) Spec() Spec { return f.spec }
func (f *fakeTool) Run(ctx context.Context, c Call, progress func()) Result {
	return f.run(ctx, c, progress)
}

// waitDone waits for the next OnDone result, failing the test when none
// arrives in 3 s.
func waitDone(t *testing.T, ch chan Result) Result {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not arrive")
		return Result{}
	}
}

// waitRunning polls until opID appears in Running, or fails.
func waitRunning(t *testing.T, r *Runner, opID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, id := range r.Running() {
			if id == opID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("op %s never showed as running", opID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartReturnsAtOnce: the tool sleeps 200 ms, so Start must come
// back well before that, and OnDone must carry the tool's result.
func TestStartReturnsAtOnce(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "sleep", Class: Normal}}
	tool.run = func(ctx context.Context, c Call, progress func()) Result {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
		}
		return Result{Status: agentjob.OpDone, Output: "slept"}
	}
	doneCh := make(chan Result, 1)
	r := &Runner{
		Registry: NewRegistry(tool),
		OnDone:   func(jobID, opID string, res Result) { doneCh <- res },
	}

	started := time.Now()
	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "sleep"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if el := time.Since(started); el >= 50*time.Millisecond {
		t.Fatalf("Start took %v, want under 50 ms", el)
	}
	res := waitDone(t, doneCh)
	if res.Status != agentjob.OpDone || res.Output != "slept" {
		t.Fatalf("got %+v, want done with output %q", res, "slept")
	}
}

// TestTimeout: a class default and an explicit call timeout both kill
// the op with Status timeout; a Long op with no timeout runs past any
// class deadline untouched.
func TestTimeout(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "slow", Class: Normal}}
	tool.run = func(ctx context.Context, c Call, progress func()) Result {
		<-ctx.Done()
		return Result{}
	}
	long := &fakeTool{spec: Spec{Name: "long", Class: Long}}
	long.run = func(ctx context.Context, c Call, progress func()) Result {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
		return Result{Status: agentjob.OpDone, Output: "long done"}
	}
	doneCh := make(chan doneEvent, 4)
	r := &Runner{
		Registry: NewRegistry(tool, long),
		Timeouts: map[Class]time.Duration{Normal: 100 * time.Millisecond},
		OnDone:   func(jobID, opID string, res Result) { doneCh <- doneEvent{opID, res} },
	}

	// Class default: Normal 100 ms kills the sleeping op.
	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "slow"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Explicit call timeout wins: Long has no class deadline, but the
	// call asks for 100 ms and gets one.
	if err := r.Start(Call{JobID: "j1", OpID: "op2", Name: "long", Timeout: 100 * time.Millisecond}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Long with no timeout: no deadline, the op finishes normally.
	if err := r.Start(Call{JobID: "j1", OpID: "op3", Name: "long"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := map[string]string{"op1": agentjob.OpTimeout, "op2": agentjob.OpTimeout, "op3": agentjob.OpDone}
	got := map[string]Result{}
	for len(got) < 3 {
		select {
		case ev := <-doneCh:
			got[ev.opID] = ev.res
		case <-time.After(3 * time.Second):
			t.Fatalf("OnDone results not all arrived: %v", got)
		}
	}
	for opID, wantStatus := range want {
		if got[opID].Status != wantStatus {
			t.Fatalf("op %s: status %q, want %q", opID, got[opID].Status, wantStatus)
		}
	}
}

func TestCancel(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "hang", Class: Normal}}
	tool.run = func(ctx context.Context, c Call, progress func()) Result {
		<-ctx.Done()
		return Result{}
	}
	doneCh := make(chan Result, 1)
	r := &Runner{
		Registry: NewRegistry(tool),
		Timeouts: map[Class]time.Duration{Normal: 10 * time.Second},
		OnDone:   func(jobID, opID string, res Result) { doneCh <- res },
	}

	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "hang"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunning(t, r, "op1")
	if !r.Cancel("op1") {
		t.Fatal("Cancel returned false for a running op")
	}
	res := waitDone(t, doneCh)
	if res.Status != agentjob.OpCancelled {
		t.Fatalf("got %+v, want status %q", res, agentjob.OpCancelled)
	}
	if r.Cancel("op1") {
		t.Fatal("Cancel returned true for a finished op")
	}
	if r.Cancel("nope") {
		t.Fatal("Cancel returned true for an unknown op")
	}
}

// TestUnknownTool: starting a tool that is not in the registry is an
// error and starts nothing.
func TestUnknownTool(t *testing.T) {
	doneCh := make(chan Result, 1)
	r := &Runner{
		Registry: NewRegistry(),
		OnDone:   func(jobID, opID string, res Result) { doneCh <- res },
	}
	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "missing"}); err == nil {
		t.Fatal("Start of an unknown tool returned nil error")
	}
	select {
	case res := <-doneCh:
		t.Fatalf("OnDone arrived for an op that never started: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPanicIsFailed: a panic inside Run is recovered and reported as a
// failed result carrying the panic text.
func TestPanicIsFailed(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "boom", Class: Quick}}
	tool.run = func(ctx context.Context, c Call, progress func()) Result {
		panic("kaboom")
	}
	doneCh := make(chan Result, 1)
	r := &Runner{
		Registry: NewRegistry(tool),
		OnDone:   func(jobID, opID string, res Result) { doneCh <- res },
	}
	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "boom"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := waitDone(t, doneCh)
	if res.Status != agentjob.OpFailed || !strings.Contains(res.Error, "kaboom") {
		t.Fatalf("got %+v, want failed with the panic text", res)
	}
}

// TestProgressCallback: every progress() call the tool makes reaches
// OnProgress with the call's job and op ids.
func TestProgressCallback(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "chatty", Class: Normal}}
	tool.run = func(ctx context.Context, c Call, progress func()) Result {
		for i := 0; i < 3; i++ {
			progress()
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				return Result{}
			}
		}
		return Result{Status: agentjob.OpDone}
	}
	var mu sync.Mutex
	var calls []string
	doneCh := make(chan Result, 1)
	r := &Runner{
		Registry: NewRegistry(tool),
		OnProgress: func(jobID, opID string) {
			mu.Lock()
			calls = append(calls, jobID+"/"+opID)
			mu.Unlock()
		},
		OnDone: func(jobID, opID string, res Result) { doneCh <- res },
	}

	if err := r.Start(Call{JobID: "j1", OpID: "op1", Name: "chatty"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, doneCh) // the op's goroutine made all progress calls before OnDone

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("got %d progress calls, want 3: %v", len(calls), calls)
	}
	for _, c := range calls {
		if c != "j1/op1" {
			t.Fatalf("progress call with wrong ids %q", c)
		}
	}
}

// TestSpecsSorted: Specs returns every registered spec, sorted by name.
func TestSpecsSorted(t *testing.T) {
	reg := NewRegistry(
		&fakeTool{spec: Spec{Name: "zeta", Class: Long, NeedsGPU: true}},
		&fakeTool{spec: Spec{Name: "alpha", Class: Quick, ExpectFast: true}},
		&fakeTool{spec: Spec{Name: "mid", Class: Normal}},
	)
	specs := reg.Specs()
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
	for i, want := range []string{"alpha", "mid", "zeta"} {
		if specs[i].Name != want {
			t.Fatalf("specs[%d].Name = %q, want %q", i, specs[i].Name, want)
		}
	}
	if !specs[0].ExpectFast || !specs[2].NeedsGPU {
		t.Fatalf("spec fields lost: %+v", specs)
	}
}
