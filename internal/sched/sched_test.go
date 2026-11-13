package sched

import (
	"testing"
	"time"
)

func TestNames(t *testing.T) {
	if got := QueueName(Weekly); got != "sched.weekly" {
		t.Errorf("QueueName(Weekly) = %q", got)
	}
	if got := RoutingKey(Weekly); got != "sched.weekly.tick" {
		t.Errorf("RoutingKey(Weekly) = %q", got)
	}
	for _, c := range All {
		if !Known(c) {
			t.Errorf("Known(%q) = false", c)
		}
	}
	if Known(Cadence("yearly")) {
		t.Error("Known(yearly) = true, want false")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tt
}

func TestBoundaryKeys(t *testing.T) {
	// 2026-11-09 is a Monday.
	mon := mustTime(t, "2026-11-09 10:00")
	sun := mustTime(t, "2026-11-08 23:59")
	monMid := mustTime(t, "2026-11-09 00:00")

	if DayKey(sun) == DayKey(monMid) {
		t.Error("DayKey did not change across midnight")
	}
	if DayKey(monMid) != DayKey(mon) {
		t.Error("DayKey changed within the same day")
	}

	if HourKey(mustTime(t, "2026-11-09 10:59")) == HourKey(mustTime(t, "2026-11-09 11:00")) {
		t.Error("HourKey did not change across the hour")
	}

	// Weekly: anchor Monday. Sun 23:59 -> most recent Monday is 2026-11-02.
	// Mon 00:00 -> most recent Monday is 2026-11-09. Different.
	if WeekKey(sun, time.Monday) == WeekKey(monMid, time.Monday) {
		t.Error("WeekKey(Monday) did not change into Monday")
	}
	// Within the week it is stable.
	if WeekKey(mustTime(t, "2026-11-09 00:00"), time.Monday) != WeekKey(mustTime(t, "2026-11-14 09:00"), time.Monday) {
		t.Error("WeekKey(Monday) changed mid-week")
	}
	// Anchor Sunday: 2026-11-08 is a Sunday.
	if WeekKey(sun, time.Sunday) != "2026-11-08" {
		t.Errorf("WeekKey(Sunday) on a Sunday = %q, want 2026-11-08", WeekKey(sun, time.Sunday))
	}

	if MonthKey(mustTime(t, "2026-11-30 23:59")) == MonthKey(mustTime(t, "2026-12-01 00:00")) {
		t.Error("MonthKey did not change across the month")
	}
}

func TestFired(t *testing.T) {
	anchor := time.Monday
	// A fresh process records its first snapshot and fires nothing.
	first := At(mustTime(t, "2026-11-09 10:00"), anchor)
	if got := (Snapshot{}).Fired(first); len(got) != 0 {
		t.Errorf("zero snapshot fired %v, want none", got)
	}
	// No boundary crossed an hour later.
	if got := first.Fired(At(mustTime(t, "2026-11-09 10:30"), anchor)); len(got) != 0 {
		t.Errorf("intra-hour poll fired %v, want none", got)
	}
	// Crossing the hour fires only hourly.
	if got := first.Fired(At(mustTime(t, "2026-11-09 11:00"), anchor)); len(got) != 1 || got[0] != Hourly {
		t.Errorf("hour crossing fired %v, want [hourly]", got)
	}
	// Monday midnight is hourly + daily + weekly (the month is not).
	prev := At(mustTime(t, "2026-11-08 23:00"), anchor)
	cur := At(mustTime(t, "2026-11-09 00:00"), anchor)
	got := prev.Fired(cur)
	if len(got) != 3 {
		t.Fatalf("Monday midnight fired %v, want 3 (hourly,daily,weekly)", got)
	}
	want := map[Cadence]bool{Hourly: true, Daily: true, Weekly: true}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected cadence %q in %v", c, got)
		}
	}
	// The 1st of a month at midnight is hourly + daily + monthly (not weekly
	// unless it is also the anchor day). 2026-12-01 is a Tuesday.
	prev = At(mustTime(t, "2026-11-30 23:00"), anchor)
	cur = At(mustTime(t, "2026-12-01 00:00"), anchor)
	got = prev.Fired(cur)
	if len(got) != 3 {
		t.Fatalf("month start fired %v, want 3 (hourly,daily,monthly)", got)
	}
	for _, c := range got {
		if c == Weekly {
			t.Errorf("weekly fired on a non-anchor month start: %v", got)
		}
	}
}

func TestAnchorWeekday(t *testing.T) {
	cases := map[string]time.Weekday{
		"":      time.Monday,
		"0":     time.Sunday,
		"1":     time.Monday,
		"6":     time.Saturday,
		"mon":   time.Monday,
		"MONDAY": time.Monday,
		"sunday": time.Sunday,
	}
	for in, want := range cases {
		got, err := AnchorWeekday(in)
		if err != nil {
			t.Errorf("AnchorWeekday(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("AnchorWeekday(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"7", "-1", "xday", "12"} {
		if _, err := AnchorWeekday(bad); err == nil {
			t.Errorf("AnchorWeekday(%q) = nil error, want error", bad)
		}
	}
}
