package kshadow

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestDryRun(t *testing.T) (*DryRun, *DryRunState) {
	t.Helper()
	dir := t.TempDir()
	st := NewDryRunState()
	d := NewDryRun("127.0.0.1:9092", nil, nil, st, filepath.Join(dir, "dry.json"), log.New(os.Stderr, "", 0))
	return d, st
}

func TestIsPriorityInversion(t *testing.T) {
	base := "voice"
	published := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		a    PendingEntry // the one that just started (starts last)
		b    matchRec     // the one that started first
		want bool
	}{
		{
			name: "high priority published first but started later",
			a:    PendingEntry{Base: base, Ts: published, Prio: 10},
			b:    matchRec{Base: base, Ts: published.Add(30 * time.Second), Prio: 0, Start: published.Add(40 * time.Second)},
			want: true,
		},
		{
			name: "same priority FIFO violation",
			a:    PendingEntry{Base: base, Ts: published, Prio: 0},
			b:    matchRec{Base: base, Ts: published.Add(30 * time.Second), Prio: 0, Start: published.Add(40 * time.Second)},
			want: true,
		},
		{
			name: "low priority started after high priority is fine",
			a:    PendingEntry{Base: base, Ts: published.Add(30 * time.Second), Prio: 0},
			b:    matchRes(matchRec{Base: base, Ts: published, Prio: 10, Start: published.Add(40 * time.Second)}),
			want: false,
		},
		{
			name: "different base queues never compare",
			a:    PendingEntry{Base: "image", Ts: published, Prio: 10},
			b:    matchRes(matchRec{Base: base, Ts: published.Add(time.Second), Prio: 0, Start: published.Add(40 * time.Second)}),
			want: false,
		},
		{
			name: "outside the concurrency window is fine",
			a:    PendingEntry{Base: base, Ts: published, Prio: 10},
			b:    matchRes(matchRec{Base: base, Ts: published.Add(10 * time.Minute), Prio: 0, Start: published.Add(10*time.Minute + time.Second)}),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := tc.b.Start.Add(time.Second) // a started after b
			if got := isPriorityInversion(&tc.a, &tc.b, start); got != tc.want {
				t.Errorf("isPriorityInversion = %v, want %v", got, tc.want)
			}
		})
	}
}

// matchRes is a tiny indirection so the test table reads left-to-right.
func matchRes(m matchRec) matchRec { return m }

func TestSweepOnceStalesPending(t *testing.T) {
	d, st := newTestDryRun(t)
	now := time.Now().UTC()
	st.Pending["job-1"] = &PendingEntry{Family: "flow", Topic: "flow.job-1", Ts: now.Add(-time.Hour)}
	st.Counter("flow").Seen = 1

	d.sweepOnce(context.Background(), now)

	if got := st.Counter("flow").Stale; got != 1 {
		t.Errorf("Stale = %d, want 1", got)
	}
	if _, ok := st.Pending["job-1"]; ok {
		t.Error("job-1 still pending after sweep")
	}
	if _, ok := st.Stale["job-1"]; !ok {
		t.Error("job-1 not in stale set")
	}
}

func TestSweepOnceCountsExpiryOnce(t *testing.T) {
	d, st := newTestDryRun(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Minute)
	st.Pending["job-2"] = &PendingEntry{
		Family: "cap", Topic: "cap.echo.default", Ts: now.Add(-2 * time.Minute), Deadline: &deadline,
	}
	st.Counter("cap").Seen = 1

	d.sweepOnce(context.Background(), now)
	d.sweepOnce(context.Background(), now)

	if got := st.Counter("cap").WouldExpire; got != 1 {
		t.Errorf("WouldExpire = %d after two sweeps, want 1 (idempotent)", got)
	}
	if got := st.Counter("cap").Stale; got != 0 {
		t.Errorf("Stale = %d, want 0 (deadline within the stale window)", got)
	}
}

func TestSweepOnceExpiryInStaleSet(t *testing.T) {
	d, st := newTestDryRun(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Minute)
	// Already stale (published long ago), deadline also passed.
	st.Stale["job-3"] = &PendingEntry{
		Family: "flow", Topic: "flow.job-3", Ts: now.Add(-time.Hour), Deadline: &deadline,
	}
	st.Counter("flow").Stale = 1

	d.sweepOnce(context.Background(), now)

	if got := st.Counter("flow").WouldExpire; got != 1 {
		t.Errorf("WouldExpire = %d, want 1", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	_, st := newTestDryRun(t)
	path := filepath.Join(t.TempDir(), "dry.json")
	st.Counter("flow").Seen = 3
	st.Offsets["events"] = 42
	deadline := time.Now().UTC()
	st.Pending["job-9"] = &PendingEntry{Family: "flow", Topic: "flow.job-9", Ts: time.Now().UTC(), Deadline: &deadline}

	if err := st.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadDryRunState(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Counter("flow").Seen; got != 3 {
		t.Errorf("flow.Seen = %d, want 3", got)
	}
	if got := loaded.Offsets["events"]; got != 42 {
		t.Errorf("offsets[events] = %d, want 42", got)
	}
	if wd, ok := loaded.Pending["job-9"]; !ok || wd.Deadline == nil || !wd.Deadline.Equal(deadline) {
		t.Errorf("pending job-9 not preserved: %+v", wd)
	}

	// A missing file yields a fresh state (kshadow's first run).
	fresh, err := LoadDryRunState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || fresh.StartedAt.IsZero() || len(fresh.Pending) != 0 {
		t.Errorf("fresh state wrong: %v %+v", err, fresh)
	}
}

func TestFlowJobIDAndPrioBase(t *testing.T) {
	if got := flowJobID("flow.chat-123"); got != "chat-123" {
		t.Errorf("flowJobID = %q", got)
	}
	if got := flowJobID("flow.chat-123.default"); got != "chat-123" {
		t.Errorf("flowJobID(.default) = %q", got)
	}
	if p, b := prioBase("voice.p10"); p != 10 || b != "voice" {
		t.Errorf("prioBase(voice.p10) = %d, %q", p, b)
	}
	if p, b := prioBase("coding.p5"); p != 5 || b != "coding" {
		t.Errorf("prioBase(coding.p5) = %d, %q", p, b)
	}
	if p, b := prioBase("fix"); p != 0 || b != "fix" {
		t.Errorf("prioBase(fix) = %d, %q", p, b)
	}
}
