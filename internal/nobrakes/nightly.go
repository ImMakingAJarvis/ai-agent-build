package nobrakes

import (
	"context"
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
)

// NightlyEligible computes the set of parked jobs the nightly gate may
// auto-enqueue (NoBrakes 4/6). The kanban task's nightly tag is the
// canonical source: for a job linked to a task (Spec.KanbanTaskID) whose
// board entry can be read, the task's Nightly flag decides — a linked job
// that the board no longer tags is not eligible, even if it still carries
// the legacy job tag. Jobs with no resolvable link (no KanbanTaskID, a
// nil board, or a task that was deleted) fall back to the job-level
// nightly tag, so jobs queued before 4/6 keep working unchanged.
func NightlyEligible(jobs *agentjob.Store, board *kanban.Store) ([]agentjob.Job, error) {
	parked, err := jobs.Parked()
	if err != nil {
		return nil, err
	}
	out := make([]agentjob.Job, 0, len(parked))
	for _, j := range parked {
		eligible := j.HasTag(agentjob.TagNightly)
		if j.KanbanTaskID != "" && board != nil {
			if t, err := board.GetTask(j.KanbanTaskID); err == nil {
				eligible = t.Nightly
			}
			// Unreadable task: keep the job-tag fallback above.
		}
		if eligible {
			out = append(out, j)
		}
	}
	return out, nil
}

// AutoEnqueue is the NoBrakes nightly gate (task 2/6): when the auto_mode
// flag is "on" and today's UTC nightly window has not been processed yet,
// it enqueues every nightly-eligible parked job (NightlyEligible, moves it
// to ready, no manual trigger) and stamps the window, so the batch runs at
// most once per UTC day and worker restarts or repeated checks cannot
// double-enqueue.
//
// The flag is read first and nothing is touched unless it is "on":
// missing or corrupt values fail safe to off. Callers treat a returned
// error as "off for this iteration" (Redis unreachable, a failed store
// write): they log a warning and keep running — a dead Redis must never
// stop the agent-run loop.
func AutoEnqueue(ctx context.Context, flags *Store, jobs *agentjob.Store, board *kanban.Store, now time.Time) (enqueued []string, on bool, err error) {
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
	eligible, err := NightlyEligible(jobs, board)
	if err != nil {
		return nil, on, err
	}
	// Enqueue first, stamp after: if the stamp fails the next iteration
	// retries, and Enqueue only ever acts on parked jobs, so the retry
	// cannot double-enqueue.
	var failed int
	for _, j := range eligible {
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
