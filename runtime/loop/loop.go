// Package loop runs the jobs whose driver is "loop" (ELR-1, M3.4, cards
// ELR-29/ELR-30/ELR-31): one tick at a time, interleaving the ready jobs
// so that a job waiting on a slow tool call does not hold its engine slot
// while it waits — the whole point of the project (gate_test.go proves it).
//
// This card (m3-4a) is the API stub plus the Gate A test, written before
// the loop exists: every method below returns "not implemented" and the
// test is skipped. Card m3-4b fills in the tick, card m3-4c the queue and
// the events, and deletes the test's Skip.
package loop

import (
	"context"
	"errors"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
)

// Store is the subset of *agentjob.Store the loop uses; card m3-4b fills
// this in.
type Store interface {
	// card m3-4b fills this in
}

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
type Loop struct {
	deps Deps
}

// New returns a Loop with its collaborators.
func New(d Deps) *Loop { return &Loop{deps: d} }

// Submit marks a job with driver loop as ready to run.
func (l *Loop) Submit(jobID string) error { return errors.New("not implemented") }

// HandleOpDone records a finished op and wakes its job when every op of
// it is done. It is called by the Runner's OnDone, from the op's
// goroutine.
func (l *Loop) HandleOpDone(jobID, opID string, res ops.Result) error {
	return errors.New("not implemented")
}

// Step runs at most ONE tick for the first ready job that can get a slot;
// ran is false when nothing could run.
func (l *Loop) Step(ctx context.Context) (ran bool, err error) {
	return false, errors.New("not implemented")
}

// IdleWhileReady is the total time jobs sat ready while no tick ran.
func (l *Loop) IdleWhileReady() time.Duration { return 0 }
