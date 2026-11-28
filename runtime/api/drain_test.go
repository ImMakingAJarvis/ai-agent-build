package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

func TestBeginIsIdempotent(t *testing.T) {
	d := NewDrainer()
	if d.Draining() {
		t.Fatal("Draining() = true before Begin")
	}
	d.Begin()
	d.Begin() // idempotent: a second Begin changes nothing
	if !d.Draining() {
		t.Fatal("Draining() = false after Begin")
	}
}

// TestDrainEndpointReportsRunningOps needs a live op: the store has a
// running op for j1 and the Runner runs it (slowTool blocks until its
// context is done).
func TestDrainEndpointReportsRunningOps(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Ingest(agentjob.Spec{ID: "j1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp("j1", agentjob.LoopOp{ID: "op1", Tool: "slow", Status: agentjob.OpRunning}); err != nil {
		t.Fatal(err)
	}
	runner := &ops.Runner{Registry: ops.NewRegistry(slowTool{})}
	if err := runner.Start(ops.Call{Name: "slow", JobID: "j1", OpID: "op1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Cancel("op1") })

	d := NewDrainer()
	s := New(Deps{Store: store, Runner: runner, Token: testToken, Version: "test", Drainer: d})
	rec := postWithToken(t, s.Handler(), "/v1/drain", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Draining   bool             `json:"draining"`
		RunningOps []map[string]any `json:"running_ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if !body.Draining {
		t.Error("draining = false, want true")
	}
	if len(body.RunningOps) != 1 ||
		body.RunningOps[0]["job"] != "j1" || body.RunningOps[0]["op"] != "op1" {
		t.Fatalf("running_ops = %v, want one entry job j1 op op1", body.RunningOps)
	}
	if !d.Draining() {
		t.Error("the endpoint did not start the Drainer")
	}
}

func TestStateShowsDraining(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	d := NewDrainer()
	s := New(Deps{Store: store, Token: testToken, Version: "test", Drainer: d})
	state := func() bool {
		rec := getWithToken(t, s.Handler(), "/v1/state")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var body struct {
			Draining bool `json:"draining"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
		}
		return body.Draining
	}
	if state() {
		t.Fatal("draining = true before Begin, want false")
	}
	d.Begin()
	if !state() {
		t.Fatal("draining = false after Begin, want true")
	}
}

func TestWaitReturnsEmptyWhenIdle(t *testing.T) {
	d := NewDrainer()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := d.Wait(ctx, func() []string { return nil })
	if len(got) != 0 {
		t.Fatalf("Wait returned %v, want empty when nothing is running", got)
	}
}

func TestWaitReturnsStillRunningOnTimeout(t *testing.T) {
	d := NewDrainer()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	still := []string{"op1", "op2"}
	start := time.Now()
	got := d.Wait(ctx, func() []string { return still })
	elapsed := time.Since(start)
	if len(got) != len(still) || got[0] != "op1" || got[1] != "op2" {
		t.Fatalf("Wait returned %v, want the still-running ops %v", got, still)
	}
	// It must block until the context ends, not give up at once.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("Wait returned after %v without waiting for the context", elapsed)
	}
}

func TestDrainRequiresToken(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postNoToken(t, s.Handler(), "/v1/drain", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
		t.Fatalf("body = %q, want the guard's 401 body", body)
	}
}
