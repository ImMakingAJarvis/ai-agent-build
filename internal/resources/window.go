package resources

// The swap-window controller's decision function (plan §3.6, rules 3–4;
// task EVB-27). The controller runs in cmd/resourced and acts through
// jarvis-swap; this file is the pure policy it implements, so the rules
// are testable without a broker, a job store or a 4090.
//
// Rule 3: never evict a working brain. A window opens only when no
// running job uses the local driver (the brain is idle, or every brain
// job is waiting).
//
// Rule 4: batch windows. Open a window when queued GPU-needing capability
// work exists and rule 3 holds. Inside a window, drain ALL queued GPU
// capability work (not just what triggered it). Close when drained, or at
// max_window, or when a brain job is ready and has waited longer than
// max_brain_wait — whichever first. A window younger than min_window does
// not close on drain (avoids thrash: each swap costs minutes of model
// load both ways).
//
// The watchdog's maintenance deadline (opened with the window by
// jarvis-swap out) remains the hard failsafe beyond max_window.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Window actions.
const (
	WindowOpen  = "open"
	WindowClose = "close"
	WindowHold  = "hold"
)

// WindowConfig is the controller's knobs (plan §7.2). The plan left the
// values open for Nathan and he did not answer in the Discord logs, so
// the plan defaults stand: 2h / 20m / 2m.
type WindowConfig struct {
	MaxWindow    time.Duration // hard cap on a window's lifetime
	MaxBrainWait time.Duration // close when a ready local job has waited this long
	MinWindow    time.Duration // a drained window younger than this holds
}

// DefaultWindowConfig returns the plan defaults (2h / 20m / 2m).
func DefaultWindowConfig() WindowConfig {
	return WindowConfig{
		MaxWindow:    2 * time.Hour,
		MaxBrainWait: 20 * time.Minute,
		MinWindow:    2 * time.Minute,
	}
}

// ReadyJob is one local (brain) job that is ready to run, with the time
// it became ready (the ready-since the close condition is measured from).
type ReadyJob struct {
	ID         string
	ReadySince time.Time
}

// WindowInput is one tick's facts about the world, collected by the
// caller. DecideWindow itself stays pure — no broker, no files, no clock.
type WindowInput struct {
	WindowOpen bool
	OpenedAt   time.Time // the open window's opened_at (the state file)
	Now        time.Time
	// CapDepth is the total depth (ready + in-flight) of each capability
	// queue that needs the brain out of the way. Zero/missing caps are
	// the same as empty.
	CapDepth map[string]int
	// RunningLocal counts local (brain) jobs in the running state —
	// rule 3.
	RunningLocal int
	// ReadyLocal are the local jobs in the ready state — rule 4's close
	// condition — with their ready times.
	ReadyLocal []ReadyJob
}

// WindowDecision is the controller's answer for one tick.
type WindowDecision struct {
	Action string   // WindowOpen | WindowClose | WindowHold
	For    []string // open: the capability queues with work (sorted)
	Reason string   // human line for logs, the window.* event and jarvis-swap
}

// DecideWindow implements rules 3–4 (see the package comment).
func DecideWindow(cfg WindowConfig, in WindowInput) WindowDecision {
	caps := make([]string, 0, len(in.CapDepth))
	total := 0
	for cap, depth := range in.CapDepth {
		if depth > 0 {
			caps = append(caps, cap)
			total += depth
		}
	}
	sort.Strings(caps)
	joined := strings.Join(caps, ", ")

	if !in.WindowOpen {
		if len(caps) > 0 && in.RunningLocal == 0 {
			return WindowDecision{
				Action: WindowOpen,
				For:    caps,
				Reason: fmt.Sprintf("queued GPU work: %s (depth %d), no local round running", joined, total),
			}
		}
		if len(caps) > 0 {
			return WindowDecision{
				Action: WindowHold,
				Reason: fmt.Sprintf("queued GPU work: %s (depth %d), but %d local round(s) running — rule 3", joined, total, in.RunningLocal),
			}
		}
		return WindowDecision{Action: WindowHold, Reason: "no queued GPU work"}
	}

	// Window open. A marker without an opened_at is stale (a lost or
	// hand-written state file): close it so the controller takes over
	// from a known state.
	if in.OpenedAt.IsZero() {
		return WindowDecision{Action: WindowClose, Reason: "stale window state (no opened_at)"}
	}
	age := in.Now.Sub(in.OpenedAt)

	// Hard cap first: max_window. The watchdog's maintenance deadline is
	// the failsafe beyond this, but the controller should close on its
	// own before the box waits for the watchdog.
	if age >= cfg.MaxWindow {
		return WindowDecision{Action: WindowClose, Reason: fmt.Sprintf("max window reached (%s)", cfg.MaxWindow)}
	}

	// Rule 4: a ready brain job that has waited past max_brain_wait ends
	// the window (it resumes the moment the brain is back).
	var brainJob string
	var brainWait time.Duration
	for _, j := range in.ReadyLocal {
		waited := in.Now.Sub(j.ReadySince)
		if waited >= cfg.MaxBrainWait && (brainJob == "" || waited > brainWait) {
			brainJob, brainWait = j.ID, waited
		}
	}
	if brainJob != "" {
		return WindowDecision{
			Action: WindowClose,
			Reason: fmt.Sprintf("brain job %s ready and waited %s (max %s)", brainJob, brainWait.Round(time.Second), cfg.MaxBrainWait),
		}
	}

	// Drained: every watched queue is empty. The window must have lived
	// at least min_window — younger than that it keeps running: the work
	// that triggered it may still be starting, and thrash costs minutes
	// of model load both ways.
	if total == 0 {
		if age >= cfg.MinWindow {
			return WindowDecision{Action: WindowClose, Reason: "drained (all capability queues empty)"}
		}
		return WindowDecision{
			Action: WindowHold,
			Reason: fmt.Sprintf("drained but younger than the minimum window (%s < %s)", age.Round(time.Second), cfg.MinWindow),
		}
	}
	return WindowDecision{
		Action: WindowHold,
		Reason: fmt.Sprintf("window open (%s), %d message(s) still queued", age.Round(time.Second), total),
	}
}
