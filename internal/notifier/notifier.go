// Package notifier sends Home Assistant notifications for job lifecycle events.
package notifier

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/notify"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	defaultDashboardAddr = "localhost:8080"
	checkpointInterval   = 10 // notify every N completed jobs
)

// Notifier sends Home Assistant notifications on job events.
type Notifier struct {
	ha         *notify.Client
	dashboard  string // e.g. "localhost:8080"
	completed  atomic.Int64
	failed     atomic.Int64
	denied     atomic.Int64
	lastCheck  atomic.Int64 // last completed count at which we sent a checkpoint
	mu         sync.Mutex
}

// New creates a notifier wired to the Home Assistant notification client.
// Reads DASHBOARD_ADDR from the environment (default localhost:8080).
func New(ha *notify.Client) *Notifier {
	addr := os.Getenv("DASHBOARD_ADDR")
	if addr == "" {
		addr = defaultDashboardAddr
	}
	return &Notifier{ha: ha, dashboard: addr}
}

// OnComplete records a job completion and sends a notification.
// Every checkpointInterval completions it also sends a checkpoint summary.
func (n *Notifier) OnComplete(rec *queue.JobRecord) {
	count := n.completed.Add(1)
	if err := n.ha.Send("Job Completed", fmt.Sprintf(
		"Job %s (%s) completed in %s via %s",
		rec.ID[:min(len(rec.ID), 8)], rec.Type, rec.Duration, rec.Model,
	)); err != nil {
		log.Printf("notify: complete: %v", err)
	}
	n.maybeCheckpoint(count)
}

// OnFail records a job failure and sends a notification.
func (n *Notifier) OnFail(rec *queue.JobRecord, err error) {
	n.failed.Add(1)
	msg := fmt.Sprintf("Job %s (%s) failed after %s: %s",
		rec.ID[:min(len(rec.ID), 8)], rec.Type, rec.Duration, rec.Model)
	if err != nil {
		msg += fmt.Sprintf("\nError: %s", err.Error())
	}
	if e := n.ha.Send("Job Failed", msg); e != nil {
		log.Printf("notify: fail: %v", e)
	}
}

// OnDenied records a job denial and sends an actionable notification with
// Approve/Reject callback URLs pointing to the dashboard API.
func (n *Notifier) OnDenied(rec *queue.JobRecord, estimatedCost float64) {
	n.denied.Add(1)
	approveURL := fmt.Sprintf("http://%s/api/approvals?id=%s&action=approve", n.dashboard, rec.ID)
	rejectURL := fmt.Sprintf("http://%s/api/approvals?id=%s&action=reject", n.dashboard, rec.ID)

	msg := fmt.Sprintf(
		"Job %s (%s) blocked: estimated cost $%.4f exceeds threshold.\nApprove: %s\nReject: %s",
		rec.ID[:min(len(rec.ID), 8)], rec.Type, estimatedCost, approveURL, rejectURL,
	)
	data := map[string]string{
		"job_id":          rec.ID,
		"job_type":        rec.Type,
		"estimated_cost":  fmt.Sprintf("%.4f", estimatedCost),
		"approve_url":     approveURL,
		"reject_url":      rejectURL,
		"dashboard_base":  fmt.Sprintf("http://%s", n.dashboard),
	}
	if e := n.ha.SendWith("Approval Needed", msg, data); e != nil {
		log.Printf("notify: denied: %v", e)
	}
}

// OnCheckpoint sends an explicit checkpoint notification with a summary.
// Useful when called externally (e.g. after a phase of agent-run completes).
func (n *Notifier) OnCheckpoint(phase string) {
	c := n.completed.Load()
	f := n.failed.Load()
	d := n.denied.Load()
	msg := fmt.Sprintf(
		"Phase %s checkpoint: %d completed, %d failed, %d denied",
		phase, c, f, d,
	)
	if e := n.ha.Send("Phase Checkpoint", msg); e != nil {
		log.Printf("notify: checkpoint: %v", e)
	}
}

func (n *Notifier) maybeCheckpoint(completed int64) {
	if completed-n.lastCheck.Load() >= checkpointInterval {
		// Try to swap; if another goroutine already took this interval, skip.
		last := n.lastCheck.Load()
		if n.lastCheck.CompareAndSwap(last, completed) {
			f := n.failed.Load()
			d := n.denied.Load()
			msg := fmt.Sprintf(
				"Checkpoint: %d completed, %d failed, %d denied (total jobs: %d)",
				completed, f, d, completed+f+d,
			)
			if err := n.ha.Send("Job Checkpoint", msg); err != nil {
				log.Printf("notify: auto-checkpoint: %v", err)
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
