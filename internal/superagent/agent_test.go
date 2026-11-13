package superagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// fakeModel replays scripted replies, one per Chat call.
type fakeModel struct {
	replies []string
	calls   int
}

func (f *fakeModel) Chat(ctx context.Context, msgs []Message) (string, error) {
	f.calls++
	if f.calls > len(f.replies) {
		return "", errors.New("no more scripted replies")
	}
	return f.replies[f.calls-1], nil
}

func testAgent(t *testing.T, dir string, replies []string) (*Agent, *fakeModel) {
	t.Helper()
	fm := &fakeModel{replies: replies}
	a := New(fm, testTools(t, dir))
	a.Log = nil
	return a, fm
}

func actionOf(tool string, args map[string]any) string {
	b, _ := json.Marshal(args)
	return "thinking...\n<ACTION>\n{\"tool\": \"" + tool + "\", \"args\": " + string(b) + "}\n</ACTION>"
}

func TestRunHappyPath(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		// plan
		`{"steps": ["write f.txt containing jarvis", "verify f.txt contains jarvis"], "done_criteria": "grep jarvis f.txt succeeds"}`,
		// step 1: write, then done
		actionOf("write_file", map[string]any{"path": "f.txt", "content": "jarvis\n"}),
		`<ACTION>{"tool": "step_done", "args": {"note": "wrote f.txt and read it back"}}</ACTION>`,
		// step 2: verify, then finish
		actionOf("shell", map[string]any{"cmd": "grep jarvis f.txt"}),
		actionOf("finish", map[string]any{"ok": true, "summary": "f.txt contains jarvis"}),
	}
	a, fm := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "make a file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK || !strings.Contains(res.Summary, "f.txt") {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatalf("f.txt not created: %v", err)
	}
	if fm.calls != 5 {
		t.Fatalf("model calls = %d, want 5", fm.calls)
	}
	if len(res.StepNotes) != 1 || res.Steps != 2 || res.Turns != 4 {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunReplan(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["old step one", "old step two"], "done_criteria": "done"}`,
		actionOf("replan", map[string]any{"reason": "step one is impossible"}),
		// re-plan
		`{"steps": ["new only step"], "done_criteria": "done"}`,
		// new step: done
		`<ACTION>{"tool": "step_done", "args": {"note": "verified"}}</ACTION>`,
		// verify
		`{"ok": true, "summary": "goal achieved"}`,
	}
	a, _ := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Plan.Steps) != 1 || res.Plan.Steps[0] != "new only step" {
		t.Fatalf("plan after replan = %+v", res.Plan)
	}
}

func TestRunStepBudget(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["a", "b", "c"], "done_criteria": "done"}`,
		// step a done
		`<ACTION>{"tool": "step_done", "args": {"note": "a ok"}}</ACTION>`,
		// step b done
		`<ACTION>{"tool": "step_done", "args": {"note": "b ok"}}</ACTION>`,
	}
	a, _ := testAgent(t, dir, replies)
	a.MaxSteps = 2
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OK {
		t.Fatalf("want failure on budget exhaustion: %+v", res)
	}
	if !strings.Contains(res.Summary, "budget") {
		t.Fatalf("summary = %q", res.Summary)
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d", res.Steps)
	}
}

func TestRunVerifierSaysNo(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["one step"], "done_criteria": "file exists"}`,
		`<ACTION>{"tool": "step_done", "args": {"note": "I think it works"}}</ACTION>`,
		`{"ok": false, "summary": "no evidence the file exists"}`,
	}
	a, _ := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OK {
		t.Fatalf("want verifier to fail the run: %+v", res)
	}
	if !strings.Contains(res.Summary, "no evidence") {
		t.Fatalf("summary = %q", res.Summary)
	}
}

func TestRunNoActionNudge(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["one step"], "done_criteria": "done"}`,
		"I forgot the block, let me try again.", // no ACTION
		`<ACTION>{"tool": "step_done", "args": {"note": "ok"}}</ACTION>`,
		`{"ok": true, "summary": "done"}`,
	}
	a, _ := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunFinishFailMidStep(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["one step"], "done_criteria": "done"}`,
		actionOf("finish", map[string]any{"ok": false, "summary": "impossible goal"}),
	}
	a, _ := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OK || !strings.Contains(res.Summary, "impossible") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunTurnBudgetExhaustsStep(t *testing.T) {
	dir := t.TempDir()
	replies := []string{
		`{"steps": ["one step"], "done_criteria": "done"}`,
		// 8 turns of shell, never step_done (MaxTurns default 8)
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		actionOf("shell", map[string]any{"cmd": "echo spin"}),
		// then the verifier
		`{"ok": false, "summary": "step never completed"}`,
	}
	a, _ := testAgent(t, dir, replies)
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OK {
		t.Fatalf("want failure: %+v", res)
	}
	if !strings.Contains(res.Summary, "step never completed") {
		t.Fatalf("summary = %q", res.Summary)
	}
}

// TestRunYield covers the capability park (EVB-32): a rabbitmq op=yield on a
// job-backed run ends the whole run as YIELDED (OK, parked) — there is no
// finish turn; the switchboard's wake is the continuation.
func TestRunYield(t *testing.T) {
	dir := t.TempDir()
	// A real job in an isolated store, so the yield op succeeds end to end.
	data := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", data)
	st, err := agentjob.Open(data)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	jobID := "evb32-run-yield"
	if _, err := st.Ingest(agentjob.Spec{ID: jobID, Title: "evb32 yield"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := st.Claim(jobID, "r1", "test"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := st.SetStatus(jobID, agentjob.State{Status: agentjob.Running, RanWith: "test", Round: "r1"}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	t.Setenv("JARVIS_JOB_ID", jobID)

	replies := []string{
		`{"steps": ["submit the work and wait for it"], "done_criteria": "the result is consumed"}`,
		actionOf("rabbitmq", map[string]any{"op": "yield", "wait-on": "c1", "note": "consume the result and finish"}),
	}
	fm := &fakeModel{replies: replies}
	a := New(fm, testTools(t, dir))
	a.Log = nil
	res, err := a.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Yielded || !res.OK {
		t.Fatalf("res = %+v, want Yielded and OK", res)
	}
	if !strings.Contains(res.Summary, "consume the result and finish") {
		t.Fatalf("summary = %q", res.Summary)
	}
	if fm.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (plan + yield)", fm.calls)
	}
	j, err := st.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != agentjob.Waiting {
		t.Fatalf("job status = %q, want waiting", j.Status)
	}
}
