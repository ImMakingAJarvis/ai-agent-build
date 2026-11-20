package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// fakeSource is a fake consumer (Job 6a): it delivers the queued messages one
// at a time, recording the delivery and the ack/nack of each. It models
// prefetch 1 — the next message is not delivered until the handler's Delivery
// is settled (acked or nacked); an unsettled first message stops the stream.
type fakeSource struct {
	msgs   []queue.Message
	events []string
}

func (f *fakeSource) Consume(handler func(Delivery)) error {
	for i, m := range f.msgs {
		body, err := json.Marshal(m)
		if err != nil {
			return err
		}
		f.events = append(f.events, fmt.Sprintf("deliver-%d", i))
		settled := false
		d := Delivery{
			Body: body,
			Ack:  func() { f.events = append(f.events, fmt.Sprintf("ack-%d", i)); settled = true },
			Nack: func() { f.events = append(f.events, fmt.Sprintf("nack-%d", i)); settled = true },
		}
		handler(d)
		if !settled {
			// prefetch 1: the broker would not deliver the next message while
			// this one is still pending, so the stream stops here.
			return nil
		}
	}
	return nil
}

// saveRec writes a reviewstate record for pr into dir (held when set).
func saveRec(t *testing.T, dir string, pr int, held bool) {
	t.Helper()
	rec := reviewstate.Record{PR: pr, Branch: "feature-x", PlaneItem: "REV-4"}
	if held {
		rec.Held = true
	}
	if err := reviewstate.Save(dir, rec); err != nil {
		t.Fatalf("save record: %v", err)
	}
}

// newWorker returns a worker whose runners are no-ops that succeed, so the
// consume tests exercise the ack/nack routing without a broker, git, or go.
func newWorker(t *testing.T, dir string) *Worker {
	t.Helper()
	w := &Worker{Queue: queue.ReviewSonnetQueue, Model: "sonnet", ModelID: "sonnet", Dir: dir}
	w.Load = reviewstate.Load
	w.Worktree = func(pr int, branch string) (string, error) { return "/wt", nil }
	w.Gate = func(dir string) GateResult { return GateResult{Pass: true} }
	w.Claude = func(args, env []string) (string, error) { return "reviewed", nil }
	return w
}

func msg(pr int) queue.Message {
	return queue.Message{
		ID:      fmt.Sprintf("mq-%d", pr),
		Payload: map[string]string{"pr": strconv.Itoa(pr), "branch": "feature-x", "reviewer": "sonnet"},
	}
}

// --- 6a. Consume -----------------------------------------------------------

// TestConsumeAcksOneAtATime delivers two messages and asserts both are acked,
// in order, with the first acked before the second is even taken (prefetch 1).
func TestConsumeAcksOneAtATime(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, false)
	saveRec(t, dir, 269, false)
	w := newWorker(t, dir)
	f := &fakeSource{msgs: []queue.Message{msg(268), msg(269)}}
	if err := w.Run(context.Background(), f); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"deliver-0", "ack-0", "deliver-1", "ack-1"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (second must not be taken until the first is acked)", f.events, want)
	}
}

// TestFakeSourcePrefetchOne asserts the fake itself enforces prefetch 1: a
// handler that never settles a delivery is not offered a second message.
func TestFakeSourcePrefetchOne(t *testing.T) {
	f := &fakeSource{msgs: []queue.Message{msg(268), msg(269)}}
	if err := f.Consume(func(Delivery) {}); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	want := []string{"deliver-0"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (an un-settled first message must stop the stream)", f.events, want)
	}
}

// TestNackOnError asserts a returned error nacks the message without requeue.
func TestNackOnError(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, false)
	w := newWorker(t, dir)
	w.Worktree = func(pr int, branch string) (string, error) { return "", fmt.Errorf("worktree failed") }
	f := &fakeSource{msgs: []queue.Message{msg(268)}}
	if err := w.Run(context.Background(), f); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"deliver-0", "nack-0"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (a returned error nacks without requeue)", f.events, want)
	}
}

// TestNackOnPanic asserts a panic in the handler nacks the message without
// requeue (the worker does not crash the consumer loop).
func TestNackOnPanic(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, false)
	w := newWorker(t, dir)
	w.Gate = func(dir string) GateResult { panic("gate exploded") }
	f := &fakeSource{msgs: []queue.Message{msg(268)}}
	if err := w.Run(context.Background(), f); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"deliver-0", "nack-0"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (a panic nacks without requeue)", f.events, want)
	}
}

// TestPoisonMessage asserts a body without a PR number is acked and logged,
// not retried — so a poison message cannot loop.
func TestPoisonMessage(t *testing.T) {
	dir := t.TempDir()
	w := newWorker(t, dir)
	// No "pr" key at all.
	f := &fakeSource{msgs: []queue.Message{{ID: "mq-p1", Payload: map[string]string{"branch": "feature-x", "reviewer": "sonnet"}}}}
	if err := w.Run(context.Background(), f); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"deliver-0", "ack-0"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (a body without a PR number is acked, not retried)", f.events, want)
	}

	// A non-numeric PR number is just as poison.
	f2 := &fakeSource{msgs: []queue.Message{{ID: "mq-p2", Payload: map[string]string{"pr": "abc", "branch": "feature-x", "reviewer": "sonnet"}}}}
	if err := w.Run(context.Background(), f2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(f2.events, want) {
		t.Errorf("events = %v, want %v (a non-numeric PR number is acked, not retried)", f2.events, want)
	}
}

// --- 6b. Worktree and the gate --------------------------------------------

// TestWorktreeFakeGit puts a stub `git` on PATH and asserts it was called with
// `worktree add` (and that the worktree path lands under BaseDir).
func TestWorktreeFakeGit(t *testing.T) {
	bin := t.TempDir()
	gitArgs := filepath.Join(bin, "git-args.txt")
	stub := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %s\nexit 0\n", gitArgs)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write git stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo := t.TempDir()
	base := t.TempDir()
	w := &Worker{RepoDir: repo, BaseDir: base}
	path, err := w.makeWorktree(268, "feature-x")
	if err != nil {
		t.Fatalf("makeWorktree: %v", err)
	}
	data, _ := os.ReadFile(gitArgs)
	s := string(data)
	if !strings.Contains(s, "worktree add") {
		t.Errorf("git not called with 'worktree add': %q", s)
	}
	if !strings.Contains(s, "feature-x") {
		t.Errorf("git worktree add missing the branch: %q", s)
	}
	if !strings.HasPrefix(path, base) {
		t.Errorf("worktree path %q is not under BaseDir %q", path, base)
	}
}

// TestGateCommands puts a stub `go` on PATH and asserts the gate runs
// `go test ./...` then `go vet ./...`, reporting the failing step.
func TestGateCommands(t *testing.T) {
	bin := t.TempDir()
	goArgs := filepath.Join(bin, "go-args.txt")
	// `go test` passes, `go vet` fails, so both run and the gate says "vet".
	stub := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nif [ \"$1\" = vet ]; then exit 1; fi\nexit 0\n", goArgs)
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write go stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	w := &Worker{}
	res := w.defaultGate(t.TempDir()) // an existing dir, so the stub actually runs
	if res.Pass {
		t.Error("expected the gate to fail (stub go vet exits 1)")
	}
	if res.Step != "vet" {
		t.Errorf("failing step = %q, want vet", res.Step)
	}
	data, _ := os.ReadFile(goArgs)
	s := string(data)
	if !strings.Contains(s, "test ./...") {
		t.Errorf("go not called with 'test ./...': %q", s)
	}
	if !strings.Contains(s, "vet ./...") {
		t.Errorf("go not called with 'vet ./...': %q", s)
	}
}

// TestGateRunsInWorktree asserts the gate runs in the worktree, not the live
// checkout.
func TestGateRunsInWorktree(t *testing.T) {
	dir := t.TempDir()
	base := t.TempDir()
	repo := t.TempDir()
	saveRec(t, dir, 268, false)
	w := &Worker{Queue: queue.ReviewSonnetQueue, Model: "sonnet", ModelID: "sonnet", Dir: dir, BaseDir: base, RepoDir: repo}
	w.Load = reviewstate.Load
	w.Worktree = func(pr int, branch string) (string, error) { return filepath.Join(base, fmt.Sprintf("pr-%d", pr)), nil }
	var gateDir string
	w.Gate = func(d string) GateResult { gateDir = d; return GateResult{Pass: true} }
	w.Claude = func(args, env []string) (string, error) { return "ok", nil }

	body, _ := json.Marshal(msg(268))
	if _, err := w.handle(body); err != nil {
		t.Fatalf("handle: %v", err)
	}
	want := filepath.Join(base, "pr-268")
	if gateDir != want {
		t.Errorf("gate ran in %q, want %q (the worktree)", gateDir, want)
	}
	if gateDir == repo {
		t.Error("gate ran in the live checkout, not the worktree")
	}
}

// TestGateFailedIsAcked asserts a non-zero gate is a failed review, not a
// crash: the message is acked.
func TestGateFailedIsAcked(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, false)
	w := newWorker(t, dir)
	w.Gate = func(dir string) GateResult { return GateResult{Pass: false, Step: "test", Out: "boom"} }
	f := &fakeSource{msgs: []queue.Message{msg(268)}}
	if err := w.Run(context.Background(), f); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"deliver-0", "ack-0"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v (a failed gate is a failed review, acked)", f.events, want)
	}
}

// TestGateTrim asserts the gate's combined output is trimmed to 4000 bytes and
// the result says the gate failed (tested with a 5000-byte fake output).
func TestGateTrim(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, false)
	w := newWorker(t, dir)
	w.Gate = func(dir string) GateResult {
		return GateResult{Pass: false, Step: "test", Out: strings.Repeat("x", 5000)}
	}
	body, _ := json.Marshal(msg(268))
	res, err := w.handle(body)
	if err != nil {
		t.Fatalf("a failed gate must ack (nil error), got %v", err)
	}
	if !res.GateFailed {
		t.Error("result.GateFailed not set")
	}
	if res.GateStep != "test" {
		t.Errorf("result.GateStep = %q, want test", res.GateStep)
	}
	if len(res.GateOutput) != gateOutputLimit {
		t.Errorf("gate output not trimmed to %d bytes: got %d", gateOutputLimit, len(res.GateOutput))
	}
}

// --- 6c. Claude, and no GPU ------------------------------------------------

// TestNoGPU asserts the cloud command's environment does not carry
// CUDA_VISIBLE_DEVICES (empty or unset) and the args contain no GPU index, and
// that the command shape matches defaultCommand's sonnet/opus path.
func TestNoGPU(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	w := &Worker{Model: "sonnet", ModelID: agentjob.ModelFor("sonnet")}
	args, env := w.claudeArgs(268, "feature-x", "REV-4")
	for _, e := range env {
		if strings.HasPrefix(e, "CUDA_VISIBLE_DEVICES=") {
			if v := strings.TrimPrefix(e, "CUDA_VISIBLE_DEVICES="); v != "" {
				t.Errorf("CUDA_VISIBLE_DEVICES not cleared: %q", e)
			}
		}
	}
	for _, a := range args {
		if isGPUIndexArg(a) {
			t.Errorf("args contain a GPU index: %q", a)
		}
	}
	wantPrefix := []string{"--model", w.ModelID, "--dangerously-skip-permissions", "-p"}
	if len(args) < len(wantPrefix) {
		t.Fatalf("args too short: %v", args)
	}
	for i, x := range wantPrefix {
		if args[i] != x {
			t.Errorf("args[%d] = %q, want %q", i, args[i], x)
		}
	}
}

// isGPUIndexArg reports whether a command arg looks like a GPU index: a known
// gpu flag, or a bare small non-negative integer.
func isGPUIndexArg(a string) bool {
	switch strings.ToLower(a) {
	case "--gpu", "-g", "--gpus", "--device", "--gpu-index":
		return true
	}
	if n, err := strconv.Atoi(a); err == nil && n >= 0 && n < 32 {
		return true
	}
	return false
}

// TestPrompt asserts the prompt is assembled from the PR number, the branch,
// and the Plane item id; it says to commit on the branch and not open a new
// pull request, and it does not say to merge.
func TestPrompt(t *testing.T) {
	w := &Worker{ModelID: "sonnet"}
	p := w.prompt(268, "feature-x", "REV-4")
	for _, want := range []string{"268", "feature-x", "REV-4"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q: %q", want, p)
		}
	}
	lower := strings.ToLower(p)
	if !strings.Contains(lower, "commit") {
		t.Errorf("prompt does not say to commit: %q", p)
	}
	if !strings.Contains(lower, "do not open a new pull request") {
		t.Errorf("prompt does not say not to open a new pull request: %q", p)
	}
	if strings.Contains(lower, "merge") {
		t.Errorf("prompt must not mention merge: %q", p)
	}
}

// TestRejectLocal asserts review.local is refused with an error that names
// "local" — by queue and by model.
func TestRejectLocal(t *testing.T) {
	wq := &Worker{Queue: queue.ReviewLocalQueue, Model: "sonnet"}
	if err := wq.Setup(); err == nil {
		t.Fatal("the review.local queue must be rejected")
	} else if !strings.Contains(err.Error(), "local") {
		t.Errorf("error %q does not contain local", err)
	}
	wm := &Worker{Queue: queue.ReviewSonnetQueue, Model: "local"}
	if err := wm.Setup(); err == nil {
		t.Fatal("model local must be rejected")
	} else if !strings.Contains(err.Error(), "local") {
		t.Errorf("error %q does not contain local", err)
	}
}

// TestHeldFable asserts a held Fable record is acked without calling claude
// (no worktree, no gate, no reviewer).
func TestHeldFable(t *testing.T) {
	dir := t.TempDir()
	saveRec(t, dir, 268, true)
	w := &Worker{Queue: queue.ReviewFableQueue, Model: "fable", ModelID: "claude-fable-5-1", Dir: dir}
	w.Load = reviewstate.Load
	var worktreeCalled, gateCalled, claudeCalled bool
	w.Worktree = func(pr int, branch string) (string, error) { worktreeCalled = true; return "", nil }
	w.Gate = func(d string) GateResult { gateCalled = true; return GateResult{} }
	w.Claude = func(args, env []string) (string, error) { claudeCalled = true; return "", nil }

	body, _ := json.Marshal(queue.Message{ID: "mq-1", Payload: map[string]string{"pr": "268", "branch": "feature-x", "reviewer": "fable"}})
	res, err := w.handle(body)
	if err != nil {
		t.Fatalf("a held review must ack (nil error), got %v", err)
	}
	if !res.Held {
		t.Error("result.Held not set")
	}
	if worktreeCalled || gateCalled || claudeCalled {
		t.Errorf("held review must not run the worktree/gate/claude (worktree=%v gate=%v claude=%v)",
			worktreeCalled, gateCalled, claudeCalled)
	}
}

// TestReviewModelFable asserts Fable resolves to claude-fable-5-1 unless
// AGENT_FABLE_MODEL overrides it, and that sonnet/opus reuse ModelFor.
func TestReviewModelFable(t *testing.T) {
	if got, err := reviewModel("fable"); err != nil || got != "claude-fable-5-1" {
		t.Errorf("reviewModel(fable) = %q, %v; want claude-fable-5-1", got, err)
	}
	t.Setenv("AGENT_FABLE_MODEL", "some-fable")
	if got, err := reviewModel("fable"); err != nil || got != "some-fable" {
		t.Errorf("reviewModel(fable) with AGENT_FABLE_MODEL = %q, %v; want some-fable", got, err)
	}
	if got, err := reviewModel("sonnet"); err != nil || got != agentjob.ModelFor("sonnet") {
		t.Errorf("reviewModel(sonnet) = %q, %v; want ModelFor(sonnet)", got, err)
	}
	if _, err := reviewModel("nope"); err == nil {
		t.Error("reviewModel(noop) should error on an unknown id")
	}
}
