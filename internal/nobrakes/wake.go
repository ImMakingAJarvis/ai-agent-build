package nobrakes

import (
	"context"
	"os/exec"
	"strings"
)

// LoopUnit is the systemd unit running the agent-run loop that consumes
// the autonomous-execution flags (the nightly gate and full auto).
const LoopUnit = "agent-run"

// UnitActive reports whether a systemd unit is currently active. It is a
// var so tests can stub it without touching systemd.
var UnitActive = func(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

// LoopRestartHelper is the installed helper (scripts/agent-run-restart)
// that starts the loop when it is down. It respects the inhibit rules:
// it does nothing while agent-run.disabled exists or resume-pending is
// set. It is a var so tests (in this package or in the packages that call
// WakeLoop) can point it at a stub.
var LoopRestartHelper = "/usr/local/bin/agent-run-restart"

// WakeLoop makes an enabled autonomous-execution flag actually take
// effect: when the agent-run loop is not active, it runs the
// agent-run-restart helper, which starts the service (and is a no-op when
// the loop is inhibited). When the loop is already active it does nothing.
//
// The returned note is owner-facing and is appended to the switch
// confirmation; an empty note means the loop was already running and
// nothing needed saying. Failures never block the flag write — the flag
// is already stored; the note just says what the owner should know.
func WakeLoop(ctx context.Context, unit string) string {
	if unit == "" {
		unit = LoopUnit
	}
	if UnitActive(unit) {
		return ""
	}
	out, err := exec.CommandContext(ctx, LoopRestartHelper).CombinedOutput()
	note := strings.TrimSpace(string(out))
	switch {
	case err != nil:
		if note == "" {
			note = err.Error()
		}
		return "The agent-run loop is down and I could not start it (" + note +
			"); this takes effect once the loop is up."
	case strings.Contains(note, "inhibited"):
		return "The agent-run loop is inhibited (agent-run.disabled exists); " +
			"remove the file and start the loop for this to run."
	case strings.Contains(note, "resume-pending"):
		return "A reboot resume owns the next loop start; this takes effect " +
			"when it runs."
	default:
		return "The agent-run loop was stopped; I started it."
	}
}
