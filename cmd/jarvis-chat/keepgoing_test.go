package main

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

func TestCutKeepGoing(t *testing.T) {
	cases := []struct {
		in        string
		on, valid bool
		isCommand bool
	}{
		{"!keepgoing on", true, true, true},
		{"!keepgoing off", false, true, true},
		{"!keepgoing ON", true, true, true}, // case-insensitive
		{"!keepgoing\tON", true, true, true},
		{"!keepgoing   on  ", true, true, true},
		{"!keepgoing", false, false, true}, // the command, but no argument
		{"!keepgoing maybe", false, false, true},
		{"!keepgoing on extra", false, false, true},
		{"!keepgoing off extra words", false, false, true},
		{"!keepgoingx", false, false, false}, // word boundary required
		{"!keepgoing2 on", false, false, false},
		{"hello !keepgoing on", false, false, false}, // must start the message
		// the other switches are different: !keepgoing must not claim them.
		{"!fullauto on", false, false, false},
		{"!automode on", false, false, false},
		// and the reverse: the other cutters must not claim !keepgoing.
		// the underscored spelling the owner tends to type
		{"!keep_going on", true, true, true},
		{"!keep_going off", false, true, true},
		{"!keep_going ON", true, true, true}, // case-insensitive
		{"!keep_going", false, false, true}, // the command, but no argument
		{"!keep_going maybe", false, false, true},
		{"!keep_going on extra", false, false, true},
		{"!keep_goingx", false, false, false}, // word boundary required
	}
	for _, c := range cases {
		on, valid, isCommand := cutKeepGoing(c.in)
		if on != c.on || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutKeepGoing(%q) = on=%v valid=%v isCommand=%v; want on=%v valid=%v isCommand=%v",
				c.in, on, valid, isCommand, c.on, c.valid, c.isCommand)
		}
		for _, cross := range []string{"!keepgoing on", "!keep_going on"} {
			if o, v, is := cutFullAuto(cross); o || v || is {
				t.Errorf("cutFullAuto(%q) claimed !keepgoing: on=%v valid=%v isCommand=%v", cross, o, v, is)
			}
			if o, v, is := cutAutoMode(cross); o || v || is {
				t.Errorf("cutAutoMode(%q) claimed !keepgoing: on=%v valid=%v isCommand=%v", cross, o, v, is)
			}
		}
	}
}

func TestHandleKeepGoingOn(t *testing.T) {
	addr, mr := miniredisURL(t)
	d := &testDiscord{}
	handleKeepGoing(d, addr, "!keepgoing on")

	if got, err := mr.Get(nobrakes.KeepGoingKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.KeepGoingKey, got, err, nobrakes.AutoModeOn)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Keep going is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "!keepgoing off") {
		t.Errorf("confirmation %q should warn how to turn it off", msg)
	}
}

func TestHandleKeepGoingOff(t *testing.T) {
	addr, mr := miniredisURL(t)
	mr.Set(nobrakes.KeepGoingKey, nobrakes.AutoModeOn) // start "on" so the test proves the clear
	d := &testDiscord{}
	// the underscored spelling, end to end
	handleKeepGoing(d, addr, "!keep_going off")

	if got, err := mr.Get(nobrakes.KeepGoingKey); err != nil || got != nobrakes.AutoModeOff {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.KeepGoingKey, got, err, nobrakes.AutoModeOff)
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "Keep going is OFF") {
		t.Errorf("confirmation %q should say OFF", msg)
	}
}

func TestHandleKeepGoingIndependentOfExecutionSwitches(t *testing.T) {
	addr, mr := miniredisURL(t)
	mr.Set(nobrakes.FullAutoKey, nobrakes.AutoModeOn) // full auto on already
	mr.Set(nobrakes.AutoModeKey, nobrakes.AutoModeOn)  // nightly on already
	d := &testDiscord{}
	handleKeepGoing(d, addr, "!keepgoing on")

	// keep_going is a modifier, not an execution switch: it must not
	// touch auto_mode or full_auto.
	if got, _ := mr.Get(nobrakes.KeepGoingKey); got != nobrakes.AutoModeOn {
		t.Fatalf("keep_going = %q; want %q", got, nobrakes.AutoModeOn)
	}
	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Fatalf("full_auto clobbered to %q", got)
	}
	if got, _ := mr.Get(nobrakes.AutoModeKey); got != nobrakes.AutoModeOn {
		t.Fatalf("auto_mode clobbered to %q", got)
	}
}

func TestHandleKeepGoingBadArgLeavesFlagUntouched(t *testing.T) {
	addr, mr := miniredisURL(t)
	for _, in := range []string{"!keepgoing", "!keepgoing maybe", "!keep_going", "!keep_going maybe"} {
		d := &testDiscord{}
		handleKeepGoing(d, addr, in)
		if msg := d.lastNotify(); !strings.Contains(msg, "Usage") {
			t.Errorf("%q: reply %q should be usage", in, msg)
		}
		if got, err := mr.Get(nobrakes.KeepGoingKey); err == nil {
			t.Errorf("%q: flag should be untouched, got %q", in, got)
		}
	}
}

func TestHandleKeepGoingRedisDown(t *testing.T) {
	d := &testDiscord{}
	// A loopback port nothing listens on: nobrakes.New fails fast.
	handleKeepGoing(d, "redis://127.0.0.1:1", "!keepgoing on")
	if msg := d.lastNotify(); !strings.Contains(msg, "can't reach") {
		t.Errorf("reply %q should say it cannot reach Redis", msg)
	}
}

func TestHandleKeepGoingDisabled(t *testing.T) {
	d := &testDiscord{}
	handleKeepGoing(d, "off", "!keepgoing on")
	if msg := d.lastNotify(); !strings.Contains(msg, "disabled") {
		t.Errorf("reply %q should say NoBrakes is disabled", msg)
	}
}
