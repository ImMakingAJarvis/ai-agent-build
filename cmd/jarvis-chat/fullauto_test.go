package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

func TestCutFullAuto(t *testing.T) {
	cases := []struct {
		in        string
		on, valid bool
		isCommand bool
	}{
		{"!fullauto on", true, true, true},
		{"!fullauto off", false, true, true},
		{"!fullauto ON", true, true, true}, // case-insensitive
		{"!fullauto\tON", true, true, true},
		{"!fullauto   on  ", true, true, true},
		{"!fullauto", false, false, true}, // the command, but no argument
		{"!fullauto maybe", false, false, true},
		{"!fullauto on extra", false, false, true},
		{"!fullauto off extra words", false, false, true},
		{"!fullautox", false, false, false}, // word boundary required
		{"!fullauto2 on", false, false, false},
		{"hello !fullauto on", false, false, false}, // must start the message
		// !automode is a different switch: !fullauto must not claim it.
		{"!automode on", false, false, false},
		{"!auto_mode on", false, false, false},
		// and the reverse: cutAutoMode must not claim !fullauto.
		// the underscored spelling the owner tends to type
		{"!full_auto on", true, true, true},
		{"!full_auto off", false, true, true},
		{"!full_auto ON", true, true, true}, // case-insensitive
		{"!full_auto", false, false, true}, // the command, but no argument
		{"!full_auto maybe", false, false, true},
		{"!full_auto on extra", false, false, true},
		{"!full_autox", false, false, false}, // word boundary required
	}
	for _, c := range cases {
		on, valid, isCommand := cutFullAuto(c.in)
		if on != c.on || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutFullAuto(%q) = on=%v valid=%v isCommand=%v; want on=%v valid=%v isCommand=%v",
				c.in, on, valid, isCommand, c.on, c.valid, c.isCommand)
		}
		for _, cross := range []string{"!fullauto on", "!full_auto on"} {
			if o, v, is := cutAutoMode(cross); o || v || is {
				t.Errorf("cutAutoMode(%q) claimed !fullauto: on=%v valid=%v isCommand=%v", cross, o, v, is)
			}
		}
	}
}

// pinLoopActive keeps handleFullAuto's wake path hermetic: the loop is
// treated as already up, so WakeLoop does nothing and never touches
// systemd or the installed helper.
func pinLoopActive(t *testing.T) {
	t.Helper()
	old := nobrakes.UnitActive
	t.Cleanup(func() { nobrakes.UnitActive = old })
	nobrakes.UnitActive = func(string) bool { return true }
}

func TestHandleFullAutoOn(t *testing.T) {
	addr, mr := miniredisURL(t)
	pinLoopActive(t)
	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on")

	if got, err := mr.Get(nobrakes.FullAutoKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.FullAutoKey, got, err, nobrakes.AutoModeOn)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Full auto is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "!fullauto off") {
		t.Errorf("confirmation %q should warn how to turn it off", msg)
	}
}

func TestHandleFullAutoOnWakesStoppedLoop(t *testing.T) {
	addr, mr := miniredisURL(t)
	// The loop is down and the restart helper (stubbed) says it started it.
	oldUnit := nobrakes.UnitActive
	oldHelper := nobrakes.LoopRestartHelper
	t.Cleanup(func() {
		nobrakes.UnitActive = oldUnit
		nobrakes.LoopRestartHelper = oldHelper
	})
	nobrakes.UnitActive = func(string) bool { return false }
	dir := t.TempDir()
	helper := filepath.Join(dir, "fake-restart")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' 'agent-run started'\n"), 0o755); err != nil {
		t.Fatalf("write stub helper: %v", err)
	}
	nobrakes.LoopRestartHelper = helper

	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on")

	if got, err := mr.Get(nobrakes.FullAutoKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.FullAutoKey, got, err, nobrakes.AutoModeOn)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Full auto is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "I started it") {
		t.Errorf("confirmation %q should report the loop was started", msg)
	}
}

func TestHandleFullAutoOff(t *testing.T) {
	addr, mr := miniredisURL(t)
	mr.Set(nobrakes.FullAutoKey, nobrakes.AutoModeOn) // start "on" so the test proves the clear
	d := &testDiscord{}
	// the underscored spelling, end to end
	handleFullAuto(d, addr, "!full_auto off")

	if got, err := mr.Get(nobrakes.FullAutoKey); err != nil || got != nobrakes.AutoModeOff {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.FullAutoKey, got, err, nobrakes.AutoModeOff)
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "Full auto is OFF") {
		t.Errorf("confirmation %q should say OFF", msg)
	}
}

func TestHandleFullAutoIndependentOfAutoMode(t *testing.T) {
	addr, mr := miniredisURL(t)
	pinLoopActive(t)
	mr.Set(nobrakes.AutoModeKey, nobrakes.AutoModeOn) // nightly on already
	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on")

	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Fatalf("full_auto = %q; want %q", got, nobrakes.AutoModeOn)
	}
	if got, _ := mr.Get(nobrakes.AutoModeKey); got != nobrakes.AutoModeOn {
		t.Fatalf("auto_mode clobbered to %q", got)
	}
}

func TestHandleFullAutoBadArgLeavesFlagUntouched(t *testing.T) {
	addr, mr := miniredisURL(t)
	for _, in := range []string{"!fullauto", "!fullauto maybe", "!full_auto", "!full_auto maybe"} {
		d := &testDiscord{}
		handleFullAuto(d, addr, in)
		if msg := d.lastNotify(); !strings.Contains(msg, "Usage") {
			t.Errorf("%q: reply %q should be usage", in, msg)
		}
		if got, err := mr.Get(nobrakes.FullAutoKey); err == nil {
			t.Errorf("%q: flag should be untouched, got %q", in, got)
		}
	}
}

func TestHandleFullAutoRedisDown(t *testing.T) {
	d := &testDiscord{}
	// A loopback port nothing listens on: nobrakes.New fails fast.
	handleFullAuto(d, "redis://127.0.0.1:1", "!fullauto on")
	if msg := d.lastNotify(); !strings.Contains(msg, "can't reach") {
		t.Errorf("reply %q should say it cannot reach Redis", msg)
	}
}

func TestHandleFullAutoDisabled(t *testing.T) {
	d := &testDiscord{}
	handleFullAuto(d, "off", "!fullauto on")
	if msg := d.lastNotify(); !strings.Contains(msg, "disabled") {
		t.Errorf("reply %q should say NoBrakes is disabled", msg)
	}
}
