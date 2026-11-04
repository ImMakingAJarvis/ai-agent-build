package nobrakes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubHelper replaces LoopRestartHelper with a stub script that prints
// out (and, when exit is set, exits non-zero).
func stubHelper(t *testing.T, out, exit string) {
	t.Helper()
	old := LoopRestartHelper
	t.Cleanup(func() { LoopRestartHelper = old })

	dir := t.TempDir()
	helper := filepath.Join(dir, "fake-restart")
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(out, "'", `'\''`) + "'"
	if exit != "" {
		script += "\nexit " + exit
	}
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub helper: %v", err)
	}
	LoopRestartHelper = helper
}

// stubUnitActive pins the systemd probe for a test.
func stubUnitActive(t *testing.T, active bool) {
	t.Helper()
	old := UnitActive
	t.Cleanup(func() { UnitActive = old })
	UnitActive = func(string) bool { return active }
}

func TestWakeLoopAlreadyActive(t *testing.T) {
	stubUnitActive(t, true)
	stubHelper(t, "should not be called", "1")

	note := WakeLoop(context.Background(), LoopUnit)
	if note != "" {
		t.Errorf("note = %q, want empty (loop already up)", note)
	}
}

func TestWakeLoopStartsStoppedLoop(t *testing.T) {
	stubUnitActive(t, false)
	stubHelper(t, "agent-run started", "")

	note := WakeLoop(context.Background(), "")
	if !strings.Contains(note, "I started it") {
		t.Errorf("note = %q, want it to report the loop was started", note)
	}
}

func TestWakeLoopInhibited(t *testing.T) {
	stubUnitActive(t, false)
	stubHelper(t, "agent-run is inhibited (agent-run.disabled exists)", "")

	note := WakeLoop(context.Background(), LoopUnit)
	if !strings.Contains(note, "inhibited") {
		t.Errorf("note = %q, want it to say the loop is inhibited", note)
	}
}

func TestWakeLoopResumePending(t *testing.T) {
	stubUnitActive(t, false)
	stubHelper(t, "deferred: resume-pending owns the next start", "")

	note := WakeLoop(context.Background(), LoopUnit)
	if !strings.Contains(note, "reboot resume owns the next loop start") {
		t.Errorf("note = %q, want it to say the reboot resume owns the start", note)
	}
}

func TestWakeLoopHelperFails(t *testing.T) {
	stubUnitActive(t, false)
	stubHelper(t, "systemctl: unit not found", "1")

	note := WakeLoop(context.Background(), LoopUnit)
	if !strings.Contains(note, "could not start it") {
		t.Errorf("note = %q, want it to report the failure", note)
	}
	if !strings.Contains(note, "unit not found") {
		t.Errorf("note = %q, want it to carry the helper output", note)
	}
}

func TestWakeLoopHelperFailsSilently(t *testing.T) {
	stubUnitActive(t, false)
	stubHelper(t, "", "1") // no output, non-zero exit

	note := WakeLoop(context.Background(), LoopUnit)
	if !strings.Contains(note, "could not start it") {
		t.Errorf("note = %q, want it to report the failure", note)
	}
	if strings.Contains(note, "()") {
		t.Errorf("note = %q, empty details should not leave empty parens", note)
	}
}
