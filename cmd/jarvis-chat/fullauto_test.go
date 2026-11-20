package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

func TestFullAuto(t *testing.T) {
	cases := []struct {
		in        string
		on        bool
		reviewer  string
		valid     bool
		isCommand bool
	}{
		{"!fullauto on sonnet", true, "sonnet", true, true},
		{"!fullauto on SONNET", true, "sonnet", true, true}, // case-insensitive, lowercased
		{"!fullauto on local", true, "local", true, true},
		{"!fullauto off", false, "", true, true},
		{"!fullauto OFF", false, "", true, true}, // case-insensitive
		{"!fullauto\toff  ", false, "", true, true},
		{"!fullauto on", false, "", false, true}, // Job 5b: a reviewer is required
		{"!fullauto", false, "", false, true},    // the command, but no argument
		{"!fullauto maybe", false, "", false, true},
		{"!fullauto on extra extra", false, "", false, true},
		{"!fullauto off extra", false, "", false, true},
		{"!fullautox on sonnet", false, "", false, false}, // word boundary required
		{"!fullauto2 on sonnet", false, "", false, false},
		{"hello !fullauto on sonnet", false, "", false, false}, // must start the message
		// !automode is a different switch: !fullauto must not claim it.
		{"!automode on", false, "", false, false},
		{"!auto_mode on", false, "", false, false},
		// the underscored spelling the owner tends to type
		{"!full_auto on sonnet", true, "sonnet", true, true},
		{"!full_auto off", false, "", true, true},
		{"!full_auto on", false, "", false, true}, // Job 5b: a reviewer is required
		{"!full_auto", false, "", false, true},    // the command, but no argument
		{"!full_auto maybe", false, "", false, true},
		{"!full_auto on extra extra", false, "", false, true},
		{"!full_autox on sonnet", false, "", false, false}, // word boundary required
	}
	for _, c := range cases {
		on, reviewer, valid, isCommand := cutFullAuto(c.in)
		if on != c.on || reviewer != c.reviewer || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutFullAuto(%q) = on=%v reviewer=%q valid=%v isCommand=%v; want on=%v reviewer=%q valid=%v isCommand=%v",
				c.in, on, reviewer, valid, isCommand, c.on, c.reviewer, c.valid, c.isCommand)
		}
		for _, cross := range []string{"!fullauto on sonnet", "!full_auto on sonnet"} {
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

// sonnetReviewerYAML is the one-reviewer block the minimal test configs
// carry: enough for reviewroute to know "sonnet".
const sonnetReviewerYAML = "  sonnet:\n    queue: review.sonnet\n"

// writeReviewConfig writes a minimal valid review config (the Job 1
// validator's required fields plus the given reviewer block) and points
// $REVIEW_CONFIG at it, so the handler's reviewer check is hermetic.
func writeReviewConfig(t *testing.T, reviewerBlock string) {
	t.Helper()
	yaml := "month_cap_usd: 10\n" +
		"max_open_prs: 2\n" +
		"max_rounds: 3\n" +
		"max_open_points: 10\n" +
		"author_job_class: build\n" +
		"reviewers:\n" +
		reviewerBlock
	path := filepath.Join(t.TempDir(), "review.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_CONFIG", path)
}

func TestHandleFullAutoOn(t *testing.T) {
	addr, mr := miniredisURL(t)
	pinLoopActive(t)
	writeReviewConfig(t, sonnetReviewerYAML)
	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on sonnet")

	if got, err := mr.Get(nobrakes.FullAutoKey); err != nil || got != nobrakes.AutoModeOn {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.FullAutoKey, got, err, nobrakes.AutoModeOn)
	}
	if got, err := mr.Get(nobrakes.FullAutoReviewerKey); err != nil || got != "sonnet" {
		t.Fatalf("redis %s = %q, err=%v; want \"sonnet\"", nobrakes.FullAutoReviewerKey, got, err)
	}
	msg := d.lastNotify()
	if !strings.Contains(msg, "Full auto is ON") {
		t.Errorf("confirmation %q should say ON", msg)
	}
	if !strings.Contains(msg, "!fullauto off") {
		t.Errorf("confirmation %q should warn how to turn it off", msg)
	}
	if !strings.Contains(msg, "sonnet") {
		t.Errorf("confirmation %q should name the stored reviewer", msg)
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
	writeReviewConfig(t, sonnetReviewerYAML)

	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on sonnet")

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
	mr.Set(nobrakes.FullAutoKey, nobrakes.AutoModeOn)    // start "on" so the test proves the clear
	mr.Set(nobrakes.FullAutoReviewerKey, "sonnet")        // and a reviewer stored
	d := &testDiscord{}
	// the underscored spelling, end to end
	handleFullAuto(d, addr, "!full_auto off")

	if got, err := mr.Get(nobrakes.FullAutoKey); err != nil || got != nobrakes.AutoModeOff {
		t.Fatalf("redis %s = %q, err=%v; want %q", nobrakes.FullAutoKey, got, err, nobrakes.AutoModeOff)
	}
	if got, err := mr.Get(nobrakes.FullAutoReviewerKey); err == nil {
		t.Errorf("redis %s = %q; !fullauto off should clear the reviewer too", nobrakes.FullAutoReviewerKey, got)
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "Full auto is OFF") {
		t.Errorf("confirmation %q should say OFF", msg)
	}
}

func TestHandleFullAutoIndependentOfAutoMode(t *testing.T) {
	addr, mr := miniredisURL(t)
	pinLoopActive(t)
	mr.Set(nobrakes.AutoModeKey, nobrakes.AutoModeOn) // nightly on already
	writeReviewConfig(t, sonnetReviewerYAML)
	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on sonnet")

	if got, _ := mr.Get(nobrakes.FullAutoKey); got != nobrakes.AutoModeOn {
		t.Fatalf("full_auto = %q; want %q", got, nobrakes.AutoModeOn)
	}
	if got, _ := mr.Get(nobrakes.AutoModeKey); got != nobrakes.AutoModeOn {
		t.Fatalf("auto_mode clobbered to %q", got)
	}
}

func TestHandleFullAutoUnknownReviewerDoesNotTurnOn(t *testing.T) {
	addr, mr := miniredisURL(t)
	writeReviewConfig(t, sonnetReviewerYAML) // fable is NOT in this config
	d := &testDiscord{}
	handleFullAuto(d, addr, "!fullauto on fable")

	if msg := d.lastNotify(); !strings.Contains(msg, "unknown reviewer") || !strings.Contains(msg, "unchanged") {
		t.Errorf("reply %q should say the name is unknown and full auto is unchanged", msg)
	}
	if got, err := mr.Get(nobrakes.FullAutoKey); err == nil {
		t.Errorf("flag set to %q; an unknown reviewer must not turn full auto on", got)
	}
	if got, err := mr.Get(nobrakes.FullAutoReviewerKey); err == nil {
		t.Errorf("reviewer key set to %q; nothing may be stored for an unknown reviewer", got)
	}
}

func TestHandleFullAutoBadArgLeavesFlagUntouched(t *testing.T) {
	addr, mr := miniredisURL(t)
	for _, in := range []string{"!fullauto", "!fullauto maybe", "!fullauto on", "!fullauto on extra extra", "!full_auto", "!full_auto maybe", "!full_auto on"} {
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
	handleFullAuto(d, "redis://127.0.0.1:1", "!fullauto on sonnet")
	if msg := d.lastNotify(); !strings.Contains(msg, "can't reach") {
		t.Errorf("reply %q should say it cannot reach Redis", msg)
	}
}

func TestHandleFullAutoDisabled(t *testing.T) {
	d := &testDiscord{}
	handleFullAuto(d, "off", "!fullauto on sonnet")
	if msg := d.lastNotify(); !strings.Contains(msg, "disabled") {
		t.Errorf("reply %q should say NoBrakes is disabled", msg)
	}
}
