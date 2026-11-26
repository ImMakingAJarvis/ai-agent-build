// metrics_test.go runs the Gate A scenario with the loop's metrics on
// (card ELR-33): the numbers the scenario must produce — three ticks
// (one tool_calls, two final), the ops gauge raised only while a tool
// runs, and, with a low SnapshotMinTokens, the snapshot saves and
// restore the interleaving causes.
package loop

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
)

// gateAEnv builds the Gate A scenario (gate_test.go) with the loop's
// metrics on and the snapshot threshold at minTokens.
func gateAEnv(t *testing.T, minTokens int) (*Loop, *agentjob.Store, *Metrics, *fake.Engine, chan struct{}) {
	t.Helper()
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
	cfg.SnapshotMinTokens = minTokens
	m, _ := NewMetrics()
	mgr := slot.New(
		slot.Config{Slots: cfg.Slots, SnapshotMinTokens: cfg.SnapshotMinTokens, OnSave: m.countSnapshotSave},
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
		Metrics:  m,
	})
	l.deps.Runner = &ops.Runner{
		Registry: reg,
		OnDone: func(jobID, opID string, res ops.Result) {
			if err := l.HandleOpDone(jobID, opID, res); err != nil {
				t.Errorf("HandleOpDone(%s, %s): %v", jobID, opID, err)
			}
		},
	}
	return l, store, m, eng, release
}

// runGateA drives the scenario to the end: A ticks and starts its slow
// tool, B ticks to done while it runs, the tool is released, and A
// finishes. afterFirstTick runs between the first and second Step.
func runGateA(t *testing.T, l *Loop, store *agentjob.Store, release chan struct{}, afterFirstTick func()) {
	t.Helper()
	ctx := context.Background()
	if err := l.Submit("a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Submit("b"); err != nil {
		t.Fatal(err)
	}
	if ran, err := l.Step(ctx); err != nil || !ran {
		t.Fatalf("step 1 = %v, %v", ran, err)
	}
	if j, _ := store.Get("a"); j.Status != agentjob.Waiting {
		t.Fatalf("after A's first tick: job a is %q, want waiting", j.Status)
	}
	if afterFirstTick != nil {
		afterFirstTick()
	}
	if ran, err := l.Step(ctx); err != nil || !ran {
		t.Fatalf("step 2 = %v, %v", ran, err)
	}
	if j, _ := store.Get("b"); j.Status != agentjob.Done {
		t.Fatalf("after B's tick: job b is %q, want done", j.Status)
	}
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
	if ran, err := l.Step(ctx); err != nil || !ran {
		t.Fatalf("step 3 = %v, %v", ran, err)
	}
}

// TestGateAMetrics: the scenario records three ticks — one tool_calls,
// two final, no errors — and the ops gauge is 1 only while A's slow
// tool runs.
func TestGateAMetrics(t *testing.T) {
	l, store, m, _, release := gateAEnv(t, config.Default().SnapshotMinTokens)

	opsDuringOp := -1.0
	runGateA(t, l, store, release, func() {
		opsDuringOp = testutil.ToFloat64(m.OpsRunning)
	})

	if got := testutil.ToFloat64(m.OpsRunning); got != 0 {
		t.Errorf("ops_running = %v after the scenario, want 0", got)
	}
	if opsDuringOp != 1 {
		t.Errorf("ops_running = %v while A's tool was running, want 1", opsDuringOp)
	}

	final := testutil.ToFloat64(m.TicksTotal.WithLabelValues(tickOutcomeFinal))
	toolCalls := testutil.ToFloat64(m.TicksTotal.WithLabelValues(tickOutcomeToolCalls))
	errTicks := testutil.ToFloat64(m.TicksTotal.WithLabelValues(tickOutcomeError))
	if got := final + toolCalls + errTicks; got != 3 {
		t.Errorf("ticks_total sums to %v (final %v, tool_calls %v, error %v), want 3", got, final, toolCalls, errTicks)
	}
	if toolCalls != 1 {
		t.Errorf("ticks with outcome tool_calls = %v, want 1", toolCalls)
	}
	if final != 2 {
		t.Errorf("ticks with outcome final = %v, want 2", final)
	}
	if errTicks != 0 {
		t.Errorf("ticks with outcome error = %v, want 0", errTicks)
	}

	// 100+100+150 prompt, 50+10+10 output, across the three ticks.
	if got := testutil.ToFloat64(m.PromptTokens); got != 350 {
		t.Errorf("prompt_tokens_total = %v, want 350", got)
	}
	if got := testutil.ToFloat64(m.OutputTokens); got != 70 {
		t.Errorf("output_tokens_total = %v, want 70", got)
	}

	if n := testutil.CollectAndCount(m.WarmHits); n != 1 {
		t.Errorf("loopd_warm_hits_total: collected %d series, want 1", n)
	}
	if got := testutil.ToFloat64(m.WarmHits); got < 0 {
		t.Errorf("warm_hits_total = %v, want >= 0", got)
	}
}

// TestGateAMetricsSnapshotSaves: with a low SnapshotMinTokens, the
// interleaving evicts both jobs once each — two saves — and A's second
// tick restores from a.bin.
func TestGateAMetricsSnapshotSaves(t *testing.T) {
	l, store, m, eng, release := gateAEnv(t, 100)

	runGateA(t, l, store, release, nil)

	if got := testutil.ToFloat64(m.SnapshotSaves); got != 2 {
		t.Errorf("snapshot_saves_total = %v, want 2 (a evicted for b, b for a)", got)
	}
	if got := len(eng.Saved()); got != 2 {
		t.Errorf("the engine saved %d snapshots, want 2: %v", got, eng.Saved())
	}
	if got := testutil.ToFloat64(m.SnapshotRestores); got != 1 {
		t.Errorf("snapshot_restores_total = %v, want 1 (a refilled from a.bin)", got)
	}
}

// TestMetricsIdleWhileReady: the idle counter counts the time the
// ready list sat non-empty while no tick was running.
func TestMetricsIdleWhileReady(t *testing.T) {
	eng := fake.New(1)
	l, _, _ := newTickEnv(t, eng, captureTool{name: "shell"})
	m, _ := NewMetrics()
	l.deps.Metrics = m
	eng.Script(tickInstructions, fake.Step{
		Reply:        engine.Message{Role: "assistant", Content: "x"},
		PromptTokens: 10, OutputTokens: 10,
	})

	now := time.Now()
	l.deps.Now = func() time.Time { return now }
	if err := l.Submit(tickJobID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if ran, err := l.Step(context.Background()); err != nil || !ran {
		t.Fatalf("step = %v, %v", ran, err)
	}
	if got := testutil.ToFloat64(m.IdleWhileReadySeconds); got != 2 {
		t.Errorf("idle_while_ready_seconds = %v, want 2", got)
	}
}
