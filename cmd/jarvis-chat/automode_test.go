package main

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
	"github.com/alicebob/miniredis/v2"
)

func TestCutAutoMode(t *testing.T) {
	cases := []struct {
		in        string
		on, valid bool
		isCommand bool
	}{
		{"!automode on", true, true, true},
		{"!automode off", false, true, true},
		{"!automode ON", true, true, true}, // case-insensitive
		{"!automode\tON", true, true, true},
		{"!automode   on  ", true, true, true},
		{"!automode", false, false, true}, // the command, but no argument
		{"!automode maybe", false, false, true},
		{"!automode on extra", false, false, true},
		{"!automode off extra words", false, false, true},
		{"!automodex", false, false, false}, // word boundary required
		{"!automode2 on", false, false, false},
		{"hello !automode on", false, false, false}, // must start the message
		{"!photo on", false, false, false},
	}
	for _, c := range cases {
		on, valid, isCommand := cutAutoMode(c.in)
		if on != c.on || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutAutoMode(%q) = on=%v valid=%v isCommand=%v; want on=%v valid=%v isCommand=%v",
				c.in, on, valid, isCommand, c.on, c.valid, c.isCommand)
		}
	}
}

// miniredisURL returns a redis:// URL for a fresh in-memory Redis.
func miniredisURL(t *testing.T) (string, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return "redis://" + mr.Addr(), mr
}

func TestHandleAutoModeOn(t *testing.T) {
	addr, mr := miniredisURL(t)
	pinLoopActive(t)
	d := &testDiscord{}
	handleAutoMode(d, addr, "!automode on")

	if got, err := mr.Get(nobrakes.AutoModeKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.AutoModeKey, got, err, nobrakes.AutoModeOn)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Auto mode is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "!automode off") {
		t.Errorf("confirmation %q should warn how to turn it off", msg)
	}
	if strings.Contains(msg, "loop is stopped") {
		t.Errorf("confirmation %q should not mention the loop (it is up)", msg)
	}
}

func TestHandleAutoModeOnLoopStopped(t *testing.T) {
	addr, mr := miniredisURL(t)
	oldUnit := nobrakes.UnitActive
	t.Cleanup(func() { nobrakes.UnitActive = oldUnit })
	nobrakes.UnitActive = func(string) bool { return false }
	d := &testDiscord{}
	handleAutoMode(d, addr, "!automode on")

	if got, err := mr.Get(nobrakes.AutoModeKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.AutoModeKey, got, err, nobrakes.AutoModeOn)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Auto mode is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "loop is stopped") {
		t.Errorf("confirmation %q should say the loop is stopped", msg)
	}
}

func TestHandleAutoModeOff(t *testing.T) {
	addr, mr := miniredisURL(t)
	mr.Set(nobrakes.AutoModeKey, nobrakes.AutoModeOn) // start "on" so the test proves the clear
	d := &testDiscord{}
	handleAutoMode(d, addr, "!automode off")

	if got, err := mr.Get(nobrakes.AutoModeKey); err != nil || got != nobrakes.AutoModeOff {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.AutoModeKey, got, err, nobrakes.AutoModeOff)
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "Auto mode is OFF") {
		t.Errorf("confirmation %q should say OFF", msg)
	}
}

func TestHandleAutoModeBadArgLeavesFlagUntouched(t *testing.T) {
	addr, mr := miniredisURL(t)
	for _, in := range []string{"!automode", "!automode maybe"} {
		d := &testDiscord{}
		handleAutoMode(d, addr, in)
		if msg := d.lastNotify(); !strings.Contains(msg, "Usage") {
			t.Errorf("%q: reply %q should be usage", in, msg)
		}
		if got, err := mr.Get(nobrakes.AutoModeKey); err == nil {
			t.Errorf("%q: flag should be untouched, got %q", in, got)
		}
	}
}

func TestHandleAutoModeRedisDown(t *testing.T) {
	d := &testDiscord{}
	// A loopback port nothing listens on: nobrakes.New fails fast.
	handleAutoMode(d, "redis://127.0.0.1:1", "!automode on")
	if msg := d.lastNotify(); !strings.Contains(msg, "can't reach") {
		t.Errorf("reply %q should say it cannot reach Redis", msg)
	}
}

func TestHandleAutoModeDisabled(t *testing.T) {
	d := &testDiscord{}
	handleAutoMode(d, "off", "!automode on")
	if msg := d.lastNotify(); !strings.Contains(msg, "disabled") {
		t.Errorf("reply %q should say NoBrakes is disabled", msg)
	}
}
