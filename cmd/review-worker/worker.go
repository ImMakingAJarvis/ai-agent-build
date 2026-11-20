// Package main is the review worker (Job 6 of docs/pr-review-build-plan.md):
// it consumes one cloud review queue (review.sonnet, review.opus or
// review.fable) and, per message, checks the pull request branch out in a
// throwaway worktree, runs the Go gate (go test ./... and go vet ./...), and —
// only if the gate passes — runs the cloud reviewer (the claude binary pinned
// to the reviewer's model, exactly the way internal/agentjob's defaultCommand
// builds the sonnet/opus command).
//
// The local queue (review.local) is NOT consumed here: a container must never
// load the host's 27B, so this binary refuses review.local. Local reviews run
// in the existing agent-run loop (a later job), in batches.
//
// Every message is acknowledged, not retried, on the outcomes the spec calls
// "done": a good review, a failed gate, a held Fable record, and a poison
// message with no PR number. A returned error or a recovered panic is the only
// path to a nack, and it nacks without requeue so a bad message cannot loop.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// gateOutputLimit caps the gate's combined output stored on a Result (Job 6b:
// trim to 4000 bytes).
const gateOutputLimit = 4000

// Delivery is one message from the review queue plus its acknowledgement
// actions. Ack settles it (it will not be redelivered); Nack rejects it
// without requeue (it does not go back to the head of the queue).
type Delivery struct {
	Body []byte
	Ack  func()
	Nack func()
}

// Source delivers review messages one at a time (prefetch 1, manual ack):
// the handler is called with each Delivery, and the next message is not
// delivered until the handler's Delivery is acked or nacked. Consume returns
// when the queue/channel closes. In production it wraps an AMQP channel; in
// tests a fake implements it.
type Source interface {
	Consume(handler func(Delivery)) error
}

// GateResult is the outcome of the gate in a worktree.
type GateResult struct {
	Pass  bool
	Step  string // "test" or "vet" — the failing step when Pass is false
	Out   string // combined output of the gate steps (untrimmed)
}

// Result is what one review message produced. It is logged for now; a later
// job publishes it back to the Plane item.
type Result struct {
	PR         int
	Branch     string
	PlaneItem  string
	Worktree   string
	GatePassed bool
	GateFailed bool
	GateStep   string
	GateOutput string // combined gate output, trimmed to gateOutputLimit
	Reviewed   bool
	ClaudeOut  string
	Poison     bool // body had no PR number; acked, not retried
	Held       bool // Fable record was held; acked without reviewing
}

// Worker consumes one review queue. The runners are function fields so tests
// can substitute a fake git (via PATH), a fake gate, and a fake claude; the
// defaults run the real commands.
type Worker struct {
	Queue   string // the review queue to consume
	Model   string // the reviewer id: sonnet, opus, or fable
	ModelID string // the resolved claude --model value (set by Setup)
	RepoDir string // the git repo the worktrees are created from
	BaseDir string // where the worktrees are created (never the live checkout)
	Dir     string // reviewstate dir (the held check)

	// Load reads the reviewstate record for a PR (defaults to
	// reviewstate.Load).
	Load func(dir string, pr int) (reviewstate.Record, error)
	// Worktree checks out the branch in a new worktree and returns its path.
	Worktree func(pr int, branch string) (string, error)
	// Gate runs go test ./... and go vet ./... in dir.
	Gate func(dir string) GateResult
	// Claude runs the cloud reviewer with the resolved args and env.
	Claude func(args, env []string) (string, error)
}

// Setup validates the required fields and resolves the claude model. It is
// called before consuming and returns an error (so the binary exits) if the
// queue is the local queue or the model is not a known cloud reviewer.
func (w *Worker) Setup() error {
	if w.Queue == "" {
		return fmt.Errorf("review-worker: -queue is required")
	}
	if w.Model == "" {
		return fmt.Errorf("review-worker: -model is required")
	}
	// The local queue is not this worker's: a container must not load the
	// host's 27B. Refuse it so the error text names "local".
	if w.Queue == queue.ReviewLocalQueue || w.Model == "local" {
		return fmt.Errorf("review-worker: the local review queue (review.local) is not consumed by this worker; local reviews run in the host agent-run loop")
	}
	m, err := reviewModel(w.Model)
	if err != nil {
		return err
	}
	w.ModelID = m
	if w.BaseDir == "" {
		w.BaseDir = defaultBaseDir()
	}
	if w.RepoDir == "" {
		w.RepoDir = "."
	}
	return nil
}

// reviewModel resolves the reviewer id to the claude --model value. Sonnet and
// Opus use agentjob.ModelFor (the same value defaultCommand uses); Fable is
// claude-fable-5-1 unless AGENT_FABLE_MODEL overrides it. local is rejected and
// an unknown id is an error.
func reviewModel(reviewer string) (string, error) {
	switch reviewer {
	case "sonnet", "opus":
		return agentjob.ModelFor(reviewer), nil
	case "fable":
		if m := os.Getenv("AGENT_FABLE_MODEL"); m != "" {
			return m, nil
		}
		return "claude-fable-5-1", nil
	case "local":
		return "", fmt.Errorf("review-worker: model local is not consumed by this worker (review.local runs in the host agent-run loop)")
	default:
		return "", fmt.Errorf("review-worker: unknown reviewer %q", reviewer)
	}
}

// Run consumes the source, routing each Delivery through deliveryHandler.
func (w *Worker) Run(ctx context.Context, src Source) error {
	return src.Consume(w.deliveryHandler)
}

// deliveryHandler processes one Delivery and settles it: it acks when handle
// returns nil (a good review, a failed gate, a held record, or a poison
// message) and nacks without requeue on a returned error or a recovered panic.
func (w *Worker) deliveryHandler(d Delivery) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("review-worker: panic processing message: %v", r)
			d.Nack() // do not requeue
		}
	}()
	res, err := w.handle(d.Body)
	if err != nil {
		log.Printf("review-worker: %v", err)
		d.Nack() // do not requeue
		return
	}
	d.Ack()
	w.logResult(res)
}

// handle runs one review message. It returns nil when the message is done with
// (acked) and an error when it should be rejected without requeue (nacked).
func (w *Worker) handle(body []byte) (Result, error) {
	var r Result
	m, err := queue.DecodeMessage(body)
	if err != nil {
		return r, fmt.Errorf("decode message: %w", err)
	}
	pr, ok := prFromMessage(m)
	if !ok {
		r.Poison = true
		log.Printf("review-worker: message %s has no PR number; acking (not retried)", m.ID)
		return r, nil
	}
	r.PR = pr
	rec, err := w.Load(w.Dir, pr)
	if err != nil {
		return r, fmt.Errorf("PR %d review record: %w", pr, err)
	}
	r.Branch = rec.Branch
	r.PlaneItem = rec.PlaneItem
	if rec.Held {
		r.Held = true
		log.Printf("review-worker: PR %d is held; acking without reviewing", pr)
		return r, nil
	}
	wt, err := w.Worktree(pr, rec.Branch)
	if err != nil {
		return r, fmt.Errorf("worktree for PR %d: %w", pr, err)
	}
	r.Worktree = wt
	g := w.Gate(wt)
	r.GateOutput = trimTo(gateOutputLimit, g.Out)
	if !g.Pass {
		r.GateFailed = true
		r.GateStep = g.Step
		log.Printf("review-worker: PR %d gate %q failed; acking (failed review)", pr, g.Step)
		return r, nil
	}
	r.GatePassed = true
	args, env := w.claudeArgs(pr, rec.Branch, rec.PlaneItem)
	out, err := w.Claude(args, env)
	if err != nil {
		return r, fmt.Errorf("claude review of PR %d: %w", pr, err)
	}
	r.Reviewed = true
	r.ClaudeOut = trimTo(gateOutputLimit, out)
	return r, nil
}

// claudeArgs builds the cloud reviewer command the same way defaultCommand does
// for sonnet and opus: the claude binary (chosen by the caller), --model from
// ModelFor, --dangerously-skip-permissions, and -p with the review prompt. The
// environment never carries CUDA_VISIBLE_DEVICES (this worker takes no GPU).
func (w *Worker) claudeArgs(pr int, branch, planeItem string) ([]string, []string) {
	args := []string{"--model", w.ModelID, "--dangerously-skip-permissions", "-p", w.prompt(pr, branch, planeItem)}
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CUDA_VISIBLE_DEVICES=") {
			continue // never hand the reviewer a GPU
		}
		env = append(env, e)
	}
	return args, env
}

// prompt assembles the review prompt from the PR number, the branch, and the
// Plane item id. It tells the model to comment on Plane and, if it changes
// code, to commit on that branch and not open a new pull request. It does not
// tell it to merge.
func (w *Worker) prompt(pr int, branch, planeItem string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing pull request #%d (branch %q).", pr, branch)
	if planeItem != "" {
		fmt.Fprintf(&b, " Post your review as a comment on the Plane item %s.", planeItem)
	} else {
		b.WriteString(" Post your review as a comment on the Plane item for this pull request.")
	}
	b.WriteString(" If you change code, commit your changes on that same branch. Do not open a new pull request.")
	return b.String()
}

// makeWorktree is the default Worktree: it runs `git worktree add` from the
// repo, creating the worktree under BaseDir (never the live checkout).
func (w *Worker) makeWorktree(pr int, branch string) (string, error) {
	path := filepath.Join(w.BaseDir, fmt.Sprintf("pr-%d", pr))
	cmd := exec.Command("git", "worktree", "add", path, branch)
	cmd.Dir = w.RepoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return path, nil
}

// defaultGate is the default Gate: it runs `go test ./...` then `go vet ./...`
// in dir (the worktree, not the live checkout) and reports the first failure.
func (w *Worker) defaultGate(dir string) GateResult {
	testOut, err1 := w.capture(dir, "go", "test", "./...")
	if err1 != nil {
		return GateResult{Pass: false, Step: "test", Out: testOut}
	}
	vetOut, err2 := w.capture(dir, "go", "vet", "./...")
	if err2 != nil {
		return GateResult{Pass: false, Step: "vet", Out: testOut + vetOut}
	}
	return GateResult{Pass: true, Out: testOut + vetOut}
}

// capture runs one command in dir and returns its combined output and error.
func (w *Worker) capture(dir string, name string, args ...string) (string, error) {
	var b bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

// runClaude is the default Claude: it runs the claude binary with the resolved
// args and env and returns the combined output and error.
func (w *Worker) runClaude(args, env []string) (string, error) {
	cmd := exec.Command("claude", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// prFromMessage extracts the PR number from a Job 4 message body. A missing or
// non-positive number means the message is a poison body (acked, not retried).
func prFromMessage(m queue.Message) (int, bool) {
	pr, err := strconv.Atoi(m.Payload["pr"])
	if err != nil || pr <= 0 {
		return 0, false
	}
	return pr, true
}

// trimTo shortens s to at most n bytes.
func trimTo(n int, s string) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// logResult records one review outcome.
func (w *Worker) logResult(r Result) {
	switch {
	case r.Poison:
		log.Printf("review-worker: poison message acked (no PR number)")
	case r.Held:
		log.Printf("review-worker: PR %d held; acked without reviewing", r.PR)
	case r.GateFailed:
		log.Printf("review-worker: PR %d gate %q failed: %s", r.PR, r.GateStep, r.GateOutput)
	case r.Reviewed:
		log.Printf("review-worker: PR %d reviewed (branch %s): %s", r.PR, r.Branch, r.ClaudeOut)
	}
}
