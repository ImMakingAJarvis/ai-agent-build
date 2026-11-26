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
	"os"
	"path/filepath"
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
// job store, the runtime config, and the clock.
type Deps struct {
	Engine   engine.Engine
	Slots    *slot.Manager
	Runner   *ops.Runner
	Registry *ops.Registry
	Store    *agentjob.Store
	Config   config.Config
	Now      func() time.Time
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
}

// New returns a Loop with its collaborators. Now defaults to time.Now.
func New(d Deps) *Loop {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Loop{deps: d}
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

// HandleOpDone records a finished op and wakes its job when every op of
// it is done. It is called by the Runner's OnDone, from the op's
// goroutine. A repeated event — an op already finished — changes nothing.
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

	// The text the model reads: the output, or the error.
	text := res.Output
	if res.Status != agentjob.OpDone {
		text = "ERROR: " + res.Error
	}
	spilled, err := tools.Spill(text, l.deps.Config.SpillBytes, func(content string) (string, error) {
		// The full text goes to <JobDir>/ops/<opID>.full; the handle is
		// its path, so the model can read it back.
		name := "ops/" + opID + ".full"
		if err := os.MkdirAll(filepath.Join(l.deps.Store.JobDir(jobID), "ops"), 0o755); err != nil {
			return "", err
		}
		if err := l.deps.Store.WriteFile(jobID, name, []byte(content)); err != nil {
			return "", err
		}
		return filepath.Join(l.deps.Store.JobDir(jobID), name), nil
	})
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

	// Every op finished: the job is ready for its next tick.
	allDone, err := l.allOpsFinished(jobID)
	if err != nil {
		return err
	}
	if !allDone {
		return nil // still waiting on the other ops
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

// Step runs at most ONE tick for the first ready job; ran is false when
// nothing ran. A job over its tick or token budget is failed and removed
// instead of ticking. On slot.ErrNoSlot the list is left as it is and
// ran is false; on success the job is removed (it is now waiting or done)
// and ran is true.
func (l *Loop) Step(ctx context.Context) (bool, error) {
	l.mu.Lock()
	if len(l.ready) == 0 {
		l.mu.Unlock()
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
		l.mu.Unlock()
		return true, nil
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
		l.mu.Unlock()
		return false, nil
	case err != nil:
		l.afterTickLocked()
		l.mu.Unlock()
		return false, err
	default:
		l.dropFirstLocked()
		l.afterTickLocked()
		l.mu.Unlock()
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

// settleIdleLocked folds the running idle interval into idle. The
// callers hold l.mu.
func (l *Loop) settleIdleLocked() {
	if !l.readySince.IsZero() {
		l.idle += l.deps.Now().Sub(l.readySince)
		l.readySince = time.Time{}
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
