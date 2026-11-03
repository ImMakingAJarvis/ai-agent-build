package nobrakes

import (
	"context"
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// AutoEnqueue is the NoBrakes nightly gate (task 2/6): when the auto_mode
// flag is "on" and today's UTC nightly window has not been processed yet,
// it enqueues every parked nightly-tagged job (moves it to ready, no
// manual trigger) and stamps the window, so the batch runs at most once
// per UTC day and worker restarts or repeated checks cannot double-enqueue.
//
// The flag is read first and nothing is touched unless it is "on":
// missing or corrupt values fail safe to off. Callers treat a returned
// error as "off for this iteration" (Redis unreachable, a failed store
// write): they log a warning and keep running — a dead Redis must never
// stop the agent-run loop.
func AutoEnqueue(ctx context.Context, flags *Store, jobs *agentjob.Store, now time.Time) (enqueued []string, on bool, err error) {
	on, err = flags.AutoModeOn(ctx)
	if err != nil {
		return nil, false, err
	}
	if !on {
		return nil, false, nil
	}
	due, err := flags.NightlyDue(ctx, now)
	if err != nil {
		return nil, on, err
	}
	if !due {
		return nil, on, nil
	}
	parked, err := jobs.ParkedNightly()
	if err != nil {
		return nil, on, err
	}
	// Enqueue first, stamp after: if the stamp fails the next iteration
	// retries, and Enqueue only ever acts on parked jobs, so the retry
	// cannot double-enqueue.
	var failed int
	for _, j := range parked {
		if err := jobs.Enqueue(j.ID); err != nil {
			failed++
			continue
		}
		enqueued = append(enqueued, j.ID)
	}
	if failed > 0 {
		// Not stamped: the window stays open so the next iteration retries
		// the jobs that failed (the ones that succeeded are ready and are
		// skipped). `jarvis-jobs enqueue <id>` is the manual escape hatch.
		return enqueued, on, fmt.Errorf("nobrakes: enqueued %d of %d parked nightly jobs", len(enqueued), len(enqueued)+failed)
	}
	// Stamp even when nothing was parked: the window has been processed,
	// and a nightly job added after the check waits for the next window —
	// that is what makes it "nightly".
	if err := flags.SetNightlyFlag(ctx, now.UTC().Format("2006-01-02")); err != nil {
		return enqueued, on, err
	}
	return enqueued, on, nil
}
