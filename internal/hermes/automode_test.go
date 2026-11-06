package hermes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// modeTestEnv points $JARVIS_REDIS at a fresh miniredis and returns the
// server for direct key assertions.
func modeTestEnv(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	t.Setenv("JARVIS_REDIS", "redis://"+mr.Addr())
	return mr
}

// stubLoop pins the wake seam for a test: unitActive says whether the
// agent-run loop is up, and helperOut is what the restart helper prints
// (nil helper output means "the helper is the real one, do not call it —
// the test must keep unitActive true").
func stubLoop(t *testing.T, unitActive bool, helperOut string) {
	t.Helper()
	unitActiveOld := nobrakes.UnitActive
	restarterOld := nobrakes.LoopRestartHelper
	t.Cleanup(func() {
		nobrakes.UnitActive = unitActiveOld
		nobrakes.LoopRestartHelper = restarterOld
	})
	nobrakes.UnitActive = func(string) bool { return unitActive }
	if helperOut != "" {
		dir := t.TempDir()
		helper := filepath.Join(dir, "fake-restart")
		script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(helperOut, "'", `'\''`) + "'"
		if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake helper: %v", err)
		}
		nobrakes.LoopRestartHelper = helper
	}
}

func TestRunSetAutoModeOn(t *testing.T) {
	mr := modeTestEnv(t)
	status, err := runSetAutoMode(context.Background(), map[string]string{"mode": "on"})
	if err != nil {
		t.Fatalf("runSetAutoMode: %v", err)
	}
	if got, _ := mr.Get(nobrakes.AutoModeKey); got != nobrakes.AutoModeOn {
		t.Errorf("redis %s = %q, want %q", nobrakes.AutoModeKey, got, nobrakes.AutoModeOn)
	}
	if !strings.Contains(status, "Auto mode is ON") {
		t.Errorf("confirmation %q should say ON", status)
	}
}

func TestRunSetAutoModeOff(t *testing.T) {
	mr := modeTestEnv(t)
	mr.Set(nobrakes.AutoModeKey, nobrakes.AutoModeOn) // start "on" so the test proves the clear
	status, err := runSetAutoMode(context.Background(), map[string]string{"mode": "off"})
	if err != nil {
		t.Fatalf("runSetAutoMode: %v", err)
	}
	if got, _ := mr.Get(nobrakes.AutoModeKey); got != nobrakes.AutoModeOff {
		t.Errorf("redis %s = %q, want %q", nobrakes.AutoModeKey, got, nobrakes.AutoModeOff)
	}
	if !strings.Contains(status, "Auto mode is OFF") {
		t.Errorf("confirmation %q should say OFF", status)
	}
}

func TestRunSetAutoModeDoesNotTouchFullAuto(t *testing.T) {
	mr := modeTestEnv(t)
	mr.Set(nobrakes.FullAutoKey, nobrakes.AutoModeOn)
	if _, err := runSetAutoMode(context.Background(), map[string]string{"mode": "on"}); err != nil {
		t.Fatalf("runSetAutoMode: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Errorf("full_auto clobbered to %q", got)
	}
}

func TestRunSetFullAutoOnWakesStoppedLoop(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, false, "agent-run started")

	status, err := runSetFullAuto(context.Background(), map[string]string{"mode": "on"})
	if err != nil {
		t.Fatalf("runSetFullAuto: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOn)
	}
	if !strings.Contains(status, "Full auto is ON") {
		t.Errorf("confirmation %q should say ON", status)
	}
	if !strings.Contains(status, "I started it") {
		t.Errorf("confirmation %q should report the loop was started", status)
	}
}

func TestRunSetFullAutoOnLoopAlreadyUp(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, true, "")

	status, err := runSetFullAuto(context.Background(), map[string]string{"mode": "on"})
	if err != nil {
		t.Fatalf("runSetFullAuto: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOn)
	}
	if !strings.Contains(status, "Full auto is ON") {
		t.Errorf("confirmation %q should say ON", status)
	}
	if strings.Contains(status, "started") || strings.Contains(status, "inhibited") {
		t.Errorf("confirmation %q should not mention the loop (it was already up)", status)
	}
}

func TestRunSetFullAutoOffDoesNotWake(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, true, "")

	status, err := runSetFullAuto(context.Background(), map[string]string{"mode": "off"})
	if err != nil {
		t.Fatalf("runSetFullAuto: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOff {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOff)
	}
	if !strings.Contains(status, "Full auto is OFF") {
		t.Errorf("confirmation %q should say OFF", status)
	}
}

func TestRunSetFullAutoInhibitedLoop(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, false, "agent-run is inhibited (agent-run.disabled)")

	status, err := runSetFullAuto(context.Background(), map[string]string{"mode": "on"})
	if err != nil {
		t.Fatalf("runSetFullAuto: %v", err)
	}
	// The flag is set anyway; the owner is told what it takes to run.
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOn)
	}
	if !strings.Contains(status, "inhibited") {
		t.Errorf("confirmation %q should say the loop is inhibited", status)
	}
}

func TestRunSetModeRedisUnreachable(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "redis://127.0.0.1:1") // nothing listens here
	for name, run := range map[string]func(context.Context, map[string]string) (string, error){
		"set_automode": runSetAutoMode,
		"set_fullauto": runSetFullAuto,
	} {
		status, err := run(context.Background(), map[string]string{"mode": "on"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(status, "can't reach") {
			t.Errorf("%s: reply %q should say it cannot reach Redis", name, status)
		}
	}
}

func TestRunSetModeDisabled(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	for name, run := range map[string]func(context.Context, map[string]string) (string, error){
		"set_automode": runSetAutoMode,
		"set_fullauto": runSetFullAuto,
	} {
		status, err := run(context.Background(), map[string]string{"mode": "on"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(status, "disabled") {
			t.Errorf("%s: reply %q should say NoBrakes is disabled", name, status)
		}
	}
}

func TestRunSetModeBadParam(t *testing.T) {
	mr := modeTestEnv(t)
	for name, run := range map[string]func(context.Context, map[string]string) (string, error){
		"set_automode": runSetAutoMode,
		"set_fullauto": runSetFullAuto,
	} {
		for _, bad := range []map[string]string{{}, {"mode": "maybe"}, {"mode": "on off"}} {
			if _, err := run(context.Background(), bad); err == nil {
				t.Errorf("%s(%v): expected an error", name, bad)
			}
		}
	}
	// A bad param must not have touched the flags.
	for _, key := range []string{nobrakes.AutoModeKey, nobrakes.FullAutoKey} {
		if got, err := mr.Get(key); err == nil {
			t.Errorf("%s = %q; flag should be untouched", key, got)
		}
	}
}

// TestProcessModeSwitchEndToEnd is the whole fast path: an owner DM
// ("turn on full auto") classifies to set_fullauto with the state
// extracted, and Execute flips the real flag — no LLM, no job scheduled.
func TestProcessModeSwitchEndToEnd(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, true, "")

	a := New(Config{AccessLevel: config.AccessFull})
	for _, act := range DefaultActions() {
		a.Register(act)
	}

	out, err := a.Process(context.Background(), "turn on full auto")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOn)
	}
	if !strings.Contains(out, "Full auto is ON") {
		t.Errorf("output %q should confirm the switch", out)
	}

	// And off again, same path.
	out, err = a.Process(context.Background(), "turn full auto off")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOff {
		t.Errorf("redis %s = %q, want %q", nobrakes.FullAutoKey, got, nobrakes.AutoModeOff)
	}
	if !strings.Contains(out, "Full auto is OFF") {
		t.Errorf("output %q should confirm the switch off", out)
	}
}

func TestProcessModeSwitchLimitedAccess(t *testing.T) {
	mr := modeTestEnv(t)
	stubLoop(t, true, "")

	a := New(Config{AccessLevel: config.AccessLimited})
	for _, act := range DefaultActions() {
		a.Register(act)
	}

	out, err := a.Process(context.Background(), "turn on full auto")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got, err := mr.Get(nobrakes.FullAutoKey); err == nil {
		t.Errorf("flag written under limited access: %q", got)
	}
	if !strings.Contains(out, "access denied") {
		t.Errorf("output %q should deny the action", out)
	}
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		ok   bool
	}{
		{"on", true, true},
		{"ON", true, true},
		{" on ", true, true},
		{"off", false, true},
		{"OFF", false, true},
		{"enable", true, true},
		{"disable", false, true},
		{"true", true, true},
		{"false", false, true},
		{"1", true, true},
		{"0", false, true},
		{"", false, false},
		{"maybe", false, false},
	}
	for _, c := range cases {
		got, err := parseMode(map[string]string{"mode": c.in})
		if c.ok {
			if err != nil {
				t.Errorf("parseMode(%q) error: %v", c.in, err)
			} else if got != c.want {
				t.Errorf("parseMode(%q) = %v, want %v", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("parseMode(%q) should error", c.in)
		}
	}
}
