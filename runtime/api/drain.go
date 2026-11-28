// drain.go is the drain state (card ELR-97, M4.1e): a Drainer loopd
// asks to stop taking new work and to report what is still running.
// Once Draining() is true the scheduler starts no new tick; a tick
// already running finishes and checkpoints (the contract card m4-1f
// builds on).
package api

import (
	"context"
	"sync"
	"time"
)

// drainPoll is how often Wait re-asks which ops are still running.
const drainPoll = 200 * time.Millisecond

// Drainer is the runtime's drain state. Begin starts the drain (once
// draining it stays draining), Draining reports it, and Wait blocks
// until no ops are running any more or the context ends.
type Drainer struct {
	mu       sync.Mutex
	draining bool
}

// NewDrainer returns a Drainer that is not draining.
func NewDrainer() *Drainer { return &Drainer{} }

// Begin starts the drain: from now on no new work is started. It is
// idempotent — a second Begin changes nothing.
func (d *Drainer) Begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining = true
}

// Draining reports whether the drain has been started.
func (d *Drainer) Draining() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.draining
}

// Wait polls running every 200 ms and returns when it reports no ops
// left (an empty slice) or when ctx ends (the op ids still running
// then).
func (d *Drainer) Wait(ctx context.Context, running func() []string) []string {
	for {
		ops := running()
		if len(ops) == 0 {
			return []string{}
		}
		select {
		case <-ctx.Done():
			return ops
		case <-time.After(drainPoll):
		}
	}
}
