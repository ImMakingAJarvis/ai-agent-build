// Package sched holds the pure scheduling primitives behind the periodic
// work queues (docs/scheduled-work.md). It decides *when* a cadence fires
// by comparing boundary-crossing keys across successive polls; it knows
// nothing about AMQP. The broker-side plumbing (queue names, the events
// exchange, EnsureSchedQueue) lives in internal/queue, and the two
// services that use it are cmd/sched-tick (the publisher) and
// cmd/sched-audit (the weekly subscriber).
package sched

import (
	"fmt"
	"time"
)

// Cadence is one periodic trigger family. Work scheduled "on the hour,
// day, week, or month" is published to the cadence's queue when its
// boundary is crossed.
type Cadence string

const (
	Hourly  Cadence = "hourly"
	Daily   Cadence = "daily"
	Weekly  Cadence = "weekly"
	Monthly Cadence = "monthly"
)

// All lists every cadence, shortest to longest.
var All = []Cadence{Hourly, Daily, Weekly, Monthly}

// State is the routing-key state segment every tick carries
// (sched.<cadence>.tick), so ParseEventRoutingKey handles a tick with no
// special case.
const State = "tick"

// QueueName is the durable queue a cadence's ticks land on.
func QueueName(c Cadence) string { return "sched." + string(c) }

// RoutingKey is the events-exchange routing key for one tick.
func RoutingKey(c Cadence) string { return QueueName(c) + "." + State }

// Known reports whether c is one of the four cadences.
func Known(c Cadence) bool {
	for _, k := range All {
		if c == k {
			return true
		}
	}
	return false
}

// HourKey changes exactly at each hour boundary (00:00, 01:00, ...).
func HourKey(t time.Time) string { return t.Format("2006-01-02 15") }

// DayKey changes exactly at midnight (the day rolls over).
func DayKey(t time.Time) string { return t.Format("2006-01-02") }

// MonthKey changes exactly at the start of a month (the 1st, 00:00).
func MonthKey(t time.Time) string { return t.Format("2006-01") }

// WeekKey is the calendar date (midnight) of the most recent anchor
// weekday on or before t. It changes exactly when the calendar crosses
// into the anchor day — the weekly boundary. With anchor Monday it turns
// over at Monday 00:00; with anchor Sunday (cron's default) at Sunday
// 00:00.
func WeekKey(t time.Time, anchor time.Weekday) string {
	d := t
	for d.Weekday() != anchor {
		d = d.AddDate(0, 0, -1)
	}
	return d.Format("2006-01-02")
}

// Snapshot is the set of boundary keys at one instant. A zero Snapshot
// means "no previous poll yet."
type Snapshot struct {
	Hour, Day, Week, Month string
}

// At captures the boundary keys of t for the given weekly anchor.
func At(t time.Time, anchor time.Weekday) Snapshot {
	return Snapshot{
		Hour:  HourKey(t),
		Day:   DayKey(t),
		Week:  WeekKey(t, anchor),
		Month: MonthKey(t),
	}
}

// Fired reports the cadences whose boundary was crossed between the
// receiver (the previous poll) and cur. A zero-valued receiver fires
// nothing: a fresh process must not replay every boundary on its first
// poll, and a service that was down across a boundary does not catch up
// (the next boundary still fires) — the same "no catch-up" rule cron
// uses. Cadences are returned shortest-to-longest; at a shared boundary
// (e.g. Monday midnight is also an hour, a day and a week) several are
// listed and each publishes to its own queue.
func (prev Snapshot) Fired(cur Snapshot) []Cadence {
	var out []Cadence
	if prev.Hour != "" && prev.Hour != cur.Hour {
		out = append(out, Hourly)
	}
	if prev.Day != "" && prev.Day != cur.Day {
		out = append(out, Daily)
	}
	if prev.Week != "" && prev.Week != cur.Week {
		out = append(out, Weekly)
	}
	if prev.Month != "" && prev.Month != cur.Month {
		out = append(out, Monthly)
	}
	return out
}

// AnchorWeekday parses the SCHED_WEEK_DAY setting into a time.Weekday.
// It accepts cron's 0-6 (Sunday=0 .. Saturday=6) or a weekday name
// (case-insensitive, "mon" is enough). An empty or invalid value is an
// error so a typo does not silently move the weekly boundary.
func AnchorWeekday(s string) (time.Weekday, error) {
	switch v := s; {
	case v == "":
		return time.Monday, nil
	default:
		if n, err := parseSmallInt(v); err == nil && n >= 0 && n <= 6 {
			return time.Weekday(n), nil
		}
		for wd := time.Sunday; wd <= time.Saturday; wd++ {
			if eqFold(v, wd.String()) || eqFold(v, wd.String()[:3]) {
				return wd, nil
			}
		}
	}
	return 0, fmt.Errorf("sched: bad week day %q: want 0-6 (Sun-Sat) or a weekday name", s)
}

func parseSmallInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
