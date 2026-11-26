// Package loop runs the jobs whose driver is "loop" (ELR-1, M3.4, cards
// ELR-29/ELR-30/ELR-31): one tick at a time, interleaving the ready jobs
// so that a job waiting on a slow tool call does not hold its engine slot
// while it waits — the whole point of the project (gate_test.go proves it).
//
// Card m3-4a wrote the API stub and the skipped Gate A test; card m3-4b
// added the tick (tick.go). Card m3-4c implemented Submit, HandleOpDone
// and Step and deleted the test's Skip: Submit keeps a FIFO of ready job
// ids and records the job's instructions as the conversation log's first
// message; HandleOpDone records a finished op (spilling a big result to a
// file), appends the tool message, and re-submits the job when every op
// of it is done; Step runs at most one tick for the first ready job,
// enforcing the tick and token budgets, and accounts the time the list
// sat non-empty with no tick running.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/tools"
)

// Deps are the loop's collaborators: the model backend, the slot manager
// that assigns engine slots to ticks, the runner that executes tool calls
// outside the model, the registry of the tools the model may call, the
// job store, the runtime config, the clock, and the metrics (a nil
// *Metrics records nothing).
type Deps struct {
	Engine   engine.Engine
	Slots    *slot.Manager
	Runner   *ops.Runner
	Registry *ops.Registry
	Store    *agentjob.Store
	Config   config.Config
	Now      func() time.Time
	Metrics  *Metrics
}

// Loop interleaves the ticks of the ready jobs.
//
// ready is the FIFO of job ids waiting for a tick, guarded by mu. For
// the idle metric: readySince is the time the list last became non-empty
// while no tick was running, and idle is the total time the list was
// non-empty while Step was not inside a tick. inTick is true from the
// moment Step settles the idle interval until the tick returns, so time
// inside a tick never counts as idle.
type Loop struct {
	deps Deps

	mu         sync.Mutex
	ready      []string
	readySince time.Time
	idle       time.Duration
	inTick     bool

	// opsMu guards opsLive: the set of op ids THIS process started in
	// the Runner. markOpFinished moves the ops gauge only for them — an
	// adopted op (M3.1b) or an unknown-tool failure finishes an op this
	// process never counted.
	opsMu   sync.Mutex
	opsLive map[string]struct{}

	// afterEdits holds the job ids whose last finished op was a
	// successful file edit (editTools) and whose next all-done pass
	// must therefore run the after_edit check (card ELR-37). Guarded by
	// mu; consumed by afterEditCheck.
	afterEdits map[string]bool
}

// New returns a Loop with its collaborators. Now defaults to time.Now.
func New(d Deps) *Loop {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Loop{deps: d}
}

// markOpStarted records that this process started the op in the Runner
// and raises the ops-running gauge. markOpFinished lowers it when one of
// THOSE ops finishes; an op this process never started (an adopted op
// after a restart, an unknown-tool failure) leaves the gauge alone.
func (l *Loop) markOpStarted(opID string) {
	l.opsMu.Lock()
	if l.opsLive == nil {
		l.opsLive = make(map[string]struct{})
	}
	l.opsLive[opID] = struct{}{}
	l.opsMu.Unlock()
	if m := l.deps.Metrics; m != nil {
		m.OpsRunning.Inc()
	}
}

// markOpFinished lowers the ops-running gauge when a started op finishes.
func (l *Loop) markOpFinished(opID string) {
	l.opsMu.Lock()
	_, live := l.opsLive[opID]
	delete(l.opsLive, opID)
	l.opsMu.Unlock()
	if live && l.deps.Metrics != nil {
		l.deps.Metrics.OpsRunning.Dec()
	}
}

// Submit marks a job with driver loop as ready to run. A job whose
// conversation log is empty gets its instructions recorded as the log's
// first message, so the log reads as a whole conversation (prompt()
// takes the user line from the spec and skips the stored one).
func (l *Loop) Submit(jobID string) error {
	msgs, err := l.deps.Store.Messages(jobID)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		j, err := l.deps.Store.Get(jobID)
		if err != nil {
			return err
		}
		content, err := json.Marshal(j.Spec.Instructions)
		if err != nil {
			return err
		}
		if _, err := l.deps.Store.AppendMessages(jobID, agentjob.LoopMessage{
			Role:    "user",
			Content: content,
		}); err != nil {
			return err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range l.ready {
		if id == jobID {
			return nil
		}
	}
	if len(l.ready) == 0 && !l.inTick {
		l.readySince = l.deps.Now()
	}
	l.ready = append(l.ready, jobID)
	return nil
}

// afterEditIDPrefix prefixes the internal check ops (card ELR-37); the
// ids are after-edit-1, after-edit-2, ... per job.
const afterEditIDPrefix = "after-edit-"

// afterEditPassed and afterEditFailed are the user messages a finished
// check op produces (card ELR-37).
const (
	afterEditPassed = "[automatic check after your edits] passed"
	afterEditFailed = "[automatic check after your edits] exit code %d\n%s"
)

// editTools are the tools whose success means the job changed files, so
// the after_edit check (card ELR-37) must run before the job ticks
// again.
var editTools = map[string]bool{"write_file": true, "edit_file": true, "apply_edits": true}

// HandleOpDone records a finished op and wakes its job when every op of
// it is done. It is called by the Runner's OnDone, from the op's
// goroutine. A repeated event — an op already finished — changes nothing.
// An op with no tool call is internal (the after_edit check): its result
// is a plain user message, not a tool message.
func (l *Loop) HandleOpDone(jobID, opID string, res ops.Result) error {
	op, err := l.deps.Store.GetOp(jobID, opID)
	if err != nil {
		return err
	}
	if op.Finished() {
		return nil
	}
	op.Status = res.Status
	result, err := json.Marshal(res)
	if err != nil {
		return err
	}
	op.Result = result
	if err := l.deps.Store.PutOp(jobID, op); err != nil {
		return err
	}
	l.markOpFinished(op.ID)

	// An internal op (no tool call) answers no model call: its result is
	// a plain user message and it wakes the job on its own (card ELR-37).
	if op.ToolCallID == "" {
		return l.finishAfterEditOp(jobID, op.ID, res)
	}

	// The text the model reads: the output, or the error.
	text := res.Output
	if res.Status != agentjob.OpDone {
		text = "ERROR: " + res.Error
	}
	spilled, err := tools.Spill(text, l.deps.Config.SpillBytes, l.spillSave(jobID, op.ID))
	if err != nil {
		return err
	}
	content, err := json.Marshal(toolContent{ToolCallID: op.ToolCallID, Content: spilled.Text})
	if err != nil {
		return err
	}
	if _, err := l.deps.Store.AppendMessages(jobID, agentjob.LoopMessage{
		Role:    "tool",
		Content: content,
	}); err != nil {
		return err
	}

	// A successful edit means this job changed files: when every op is
	// finished, the after_edit check runs before the job may tick again
	// (card ELR-37).
	if editTools[op.Tool] && res.Status == agentjob.OpDone {
		l.mu.Lock()
		if l.afterEdits == nil {
			l.afterEdits = make(map[string]bool)
		}
		l.afterEdits[jobID] = true
		l.mu.Unlock()
	}

	// Every op finished: the job is ready for its next tick — unless the
	// after_edit check runs first (card ELR-37).
	allDone, err := l.allOpsFinished(jobID)
	if err != nil {
		return err
	}
	if !allDone {
		return nil // still waiting on the other ops
	}
	chk, live, err := l.afterEditCheck(jobID)
	if err != nil {
		return err
	}
	if live {
		return nil // the other finishing op's check op is running; it wakes the job
	}
	if chk.ID != "" {
		return l.startOp(jobID, chk)
	}
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	st := j.State
	st.Status = agentjob.Ready
	st.WaitReason = ""
	if err := l.deps.Store.SetStatus(jobID, st); err != nil {
		return err
	}
	return l.Submit(jobID)
}

// afterEditCheck decides, for a job whose every op is finished, whether
// the after_edit check (card ELR-37) runs: it returns the op this caller
// must start when it created one, live when a check op already runs
// (created by an op that finished in the same instant), and (zero,
// false) when no check is due and the job may go ready. The decision and
// the op's creation run under l.mu, so two ops finishing concurrently
// create exactly one check op and no caller makes the job ready while
// one is live.
func (l *Loop) afterEditCheck(jobID string) (op agentjob.LoopOp, live bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pending := l.afterEdits[jobID]
	if pending {
		delete(l.afterEdits, jobID)
	}
	if live, err = l.liveAfterEditOp(jobID); err != nil {
		return agentjob.LoopOp{}, false, err
	}
	if live || !(pending && len(l.deps.Config.AfterEdit) > 0) {
		return op, live, nil
	}
	id, err := l.nextAfterEditID(jobID)
	if err != nil {
		return agentjob.LoopOp{}, false, err
	}
	args, err := json.Marshal(map[string]any{
		"cmd":             strings.Join(l.deps.Config.AfterEdit, " && "),
		"timeout_seconds": l.deps.Config.AfterEditTimeoutSeconds,
	})
	if err != nil {
		return agentjob.LoopOp{}, false, err
	}
	op = agentjob.LoopOp{ID: id, Tool: "shell", Args: args, Status: agentjob.OpPending}
	if err = l.deps.Store.PutOp(jobID, op); err != nil {
		return agentjob.LoopOp{}, false, err
	}
	return op, false, nil
}

// finishAfterEditOp records the result of a finished after_edit check op
// (card ELR-37) as a plain user message — the op answered no tool call,
// so a tool message would be wrong — and wakes the job: the check op was
// its last op.
func (l *Loop) finishAfterEditOp(jobID, opID string, res ops.Result) error {
	content := afterEditPassed
	if res.Status != agentjob.OpDone {
		body := res.Output
		if body == "" {
			body = res.Error
		}
		spilled, err := tools.Spill(body, l.deps.Config.SpillBytes, l.spillSave(jobID, opID))
		if err != nil {
			return err
		}
		content = fmt.Sprintf(afterEditFailed, res.ExitCode, spilled.Text)
	}
	b, err := json.Marshal(content)
	if err != nil {
		return err
	}
	if _, err := l.deps.Store.AppendMessages(jobID, agentjob.LoopMessage{
		Role:    "user",
		Content: b,
	}); err != nil {
		return err
	}
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	st := j.State
	st.Status = agentjob.Ready
	st.WaitReason = ""
	if err := l.deps.Store.SetStatus(jobID, st); err != nil {
		return err
	}
	return l.Submit(jobID)
}

// afterEditOps lists the job's ops by walking the ops directory like
// allOpsFinished: the spilled .full texts live in the same directory and
// only the .json files are ops.
func (l *Loop) afterEditOps(jobID string) ([]agentjob.LoopOp, error) {
	dir := filepath.Join(l.deps.Store.JobDir(jobID), "ops")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []agentjob.LoopOp
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		op, err := l.deps.Store.GetOp(jobID, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

// liveAfterEditOp reports whether the job has an after_edit check op
// that has not finished (card ELR-37): an op with no tool call. The
// callers hold l.mu.
func (l *Loop) liveAfterEditOp(jobID string) (bool, error) {
	ops, err := l.afterEditOps(jobID)
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op.ToolCallID == "" && !op.Finished() {
			return true, nil
		}
	}
	return false, nil
}

// nextAfterEditID returns the id of the next after_edit check op:
// after-edit-<n> with n one past the highest existing one (card
// ELR-37). The callers hold l.mu.
func (l *Loop) nextAfterEditID(jobID string) (string, error) {
	ops, err := l.afterEditOps(jobID)
	if err != nil {
		return "", err
	}
	max := 0
	for _, op := range ops {
		if !strings.HasPrefix(op.ID, afterEditIDPrefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(op.ID, afterEditIDPrefix))
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return afterEditIDPrefix + strconv.Itoa(max+1), nil
}

// spillSave returns the save function that keeps the full text of a
// spilled op result at <JobDir>/ops/<opID>.full; the spilled handle is
// the path, so the model can read it back.
func (l *Loop) spillSave(jobID, opID string) func(string) (string, error) {
	return func(content string) (string, error) {
		name := "ops/" + opID + ".full"
		if err := os.MkdirAll(filepath.Join(l.deps.Store.JobDir(jobID), "ops"), 0o755); err != nil {
			return "", err
		}
		if err := l.deps.Store.WriteFile(jobID, name, []byte(content)); err != nil {
			return "", err
		}
		return filepath.Join(l.deps.Store.JobDir(jobID), name), nil
	}
}

// failGuard fails a job for an unusable-reply or repeat run (card
// ELR-88) and removes it from the ready list, with the same bookkeeping
// as the budget path in Step: the running idle interval settles, the job
// drops off the head, the metrics settle.
func (l *Loop) failGuard(jobID string, j agentjob.Job, reason string) (bool, error) {
	st := j.State
	st.Status = agentjob.Failed
	st.Error = reason
	if err := l.deps.Store.SetStatus(jobID, st); err != nil {
		return false, err
	}
	l.mu.Lock()
	l.settleIdleLocked()
	l.dropFirstLocked()
	l.afterTickLocked()
	l.deps.Metrics.setReadyJobs(len(l.ready))
	l.mu.Unlock()
	return true, nil
}

// Step runs at most ONE tick for the first ready job; ran is false when
// nothing ran. A job over its tick or token budget is failed and removed
// instead of ticking. On slot.ErrNoSlot the list is left as it is and
// ran is false; on success the job is removed (it is now waiting or done)
// and ran is true.
func (l *Loop) Step(ctx context.Context) (bool, error) {
	l.mu.Lock()
	if len(l.ready) == 0 {
		l.mu.Unlock()
		l.deps.Metrics.setReadyJobs(0)
		return false, nil
	}
	jobID := l.ready[0]
	l.mu.Unlock()

	// The budgets: the tick cap on the conversation length, the token cap
	// on the state.
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return false, err
	}
	msgs, err := l.deps.Store.Messages(jobID)
	if err != nil {
		return false, err
	}
	if len(msgs) >= l.deps.Config.MaxTicks*2 || j.State.Tokens >= l.deps.Config.MaxTaskTokens {
		st := j.State
		st.Status = agentjob.Failed
		st.Error = "budget: tick or token limit reached"
		if err := l.deps.Store.SetStatus(jobID, st); err != nil {
			return false, err
		}
		l.mu.Lock()
		l.settleIdleLocked()
		l.dropFirstLocked()
		l.afterTickLocked()
		l.deps.Metrics.setReadyJobs(len(l.ready))
		l.mu.Unlock()
		return true, nil
	}

	// Unusable replies (card ELR-88): the runtime notes appended in a row
	// at the end of the log. The model has been answered that many times
	// and still cannot produce a usable reply — fail the job instead of
	// burning more ticks.
	if bad := trailingBad(msgs); bad >= l.deps.Config.MaxBadReplies {
		return l.failGuard(jobID, j, fmt.Sprintf("model gave %d unusable replies in a row", bad))
	}

	// Repeated tool calls (card ELR-88): the identical calls at the end
	// of the log. At repeat_warn the model gets one warning note; at
	// repeat_fail the job is failed.
	same := trailingSame(msgs)
	if same >= l.deps.Config.RepeatFail {
		return l.failGuard(jobID, j, fmt.Sprintf("model repeated the same tool call %d times", same))
	}
	if same == l.deps.Config.RepeatWarn && !isRepeatWarn(msgs, same) {
		if err := l.appendUserNote(jobID, repeatWarnNote(same)); err != nil {
			return false, err
		}
	}

	// A tick starts: the running idle interval ends here, and time inside
	// the tick never counts as idle.
	l.mu.Lock()
	l.settleIdleLocked()
	l.inTick = true
	l.mu.Unlock()

	err = l.tick(ctx, jobID)

	l.mu.Lock()
	l.inTick = false
	switch {
	case errors.Is(err, slot.ErrNoSlot):
		// Nothing ran; the job stays first in the list.
		l.afterTickLocked()
		l.deps.Metrics.setReadyJobs(len(l.ready))
		l.mu.Unlock()
		return false, nil
	case err != nil:
		l.afterTickLocked()
		l.deps.Metrics.setReadyJobs(len(l.ready))
		l.mu.Unlock()
		return false, err
	default:
		l.dropFirstLocked()
		l.afterTickLocked()
		l.deps.Metrics.setReadyJobs(len(l.ready))
		l.mu.Unlock()
		// A tick can end with the job ready again INSIDE the tick: an
		// empty reply (card ELR-88) or a tool call that failed before it
		// started (unknown tool, invalid arguments). Their resubmit was a
		// no-op while the job was still first in the list, and the drop
		// above removed that entry — put it back, or the loop would never
		// tick the job again.
		if j, err := l.deps.Store.Get(jobID); err == nil && j.State.Status == agentjob.Ready {
			_ = l.Submit(jobID)
		}
		return true, nil
	}
}

// allOpsFinished reports whether every op of the job has finished. It
// walks the ops directory itself and reads only the .json op files: the
// spilled <opID>.full texts and the .log streams live in the same
// directory and are not ops (Store.Ops would try to parse them).
func (l *Loop) allOpsFinished(jobID string) (bool, error) {
	dir := filepath.Join(l.deps.Store.JobDir(jobID), "ops")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil // no ops at all — nothing to wait for
		}
		return false, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		op, err := l.deps.Store.GetOp(jobID, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return false, err
		}
		if !op.Finished() {
			return false, nil
		}
	}
	return true, nil
}

// dropFirstLocked removes the first ready job — the one this Step
// processed. The callers hold l.mu.
func (l *Loop) dropFirstLocked() {
	if len(l.ready) > 0 {
		l.ready = l.ready[1:]
	}
}

// afterTickLocked ends (or restarts) the idle interval after a tick or a
// failed attempt: no tick is running, so the list being non-empty means
// idle time accumulates again from now. The callers hold l.mu.
func (l *Loop) afterTickLocked() {
	if len(l.ready) > 0 {
		l.readySince = l.deps.Now()
	} else {
		l.readySince = time.Time{}
	}
}

// settleIdleLocked folds the running idle interval into idle and, with
// it, into the idle counter. The callers hold l.mu.
func (l *Loop) settleIdleLocked() {
	if !l.readySince.IsZero() {
		d := l.deps.Now().Sub(l.readySince)
		l.idle += d
		l.readySince = time.Time{}
		l.deps.Metrics.addIdle(d)
	}
}

// IdleWhileReady is the total time jobs sat ready while no tick ran.
func (l *Loop) IdleWhileReady() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.idle
	if !l.readySince.IsZero() {
		d += l.deps.Now().Sub(l.readySince)
	}
	return d
}
