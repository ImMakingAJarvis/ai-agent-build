package nobrakes

import (
	"context"
	"testing"
)

// aliveByID reports whether the job with the given id is still running,
// per the map passed in (missing id = dead). Stands in for the job-store
// liveness check in cmd/jarvis-jobs.
func aliveByID(ids map[string]bool) func(string) bool {
	return func(id string) bool { return ids[id] }
}

func TestAcquireRunningFreeKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	holder, ok, err := s.AcquireRunning(ctx, "job-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || holder != "job-a" {
		t.Fatalf("want (job-a, true), got (%q, %v)", holder, ok)
	}
	if got, err := s.RunningHolder(ctx); err != nil || got != "job-a" {
		t.Fatalf("holder after acquire: %q, %v", got, err)
	}
}

func TestAcquireRunningHeldByLiveJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, err := s.AcquireRunning(ctx, "job-a", nil); err != nil || !ok {
		t.Fatal(err, ok)
	}
	// job-a is alive: job-b must be reported, not acquire.
	holder, ok, err := s.AcquireRunning(ctx, "job-b", aliveByID(map[string]bool{"job-a": true}))
	if err != nil {
		t.Fatal(err)
	}
	if ok || holder != "job-a" {
		t.Fatalf("want held by job-a, got (%q, %v)", holder, ok)
	}
	if got, _ := s.RunningHolder(ctx); got != "job-a" {
		t.Fatalf("marker changed: %q", got)
	}
}

func TestAcquireRunningReentrant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("first acquire failed")
	}
	// The same job re-acquires (a round resumed after a reboot): it is
	// always allowed, even with no liveness info at all.
	holder, ok, err := s.AcquireRunning(ctx, "job-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || holder != "job-a" {
		t.Fatalf("reentrant acquire: want (job-a, true), got (%q, %v)", holder, ok)
	}
}

func TestAcquireRunningStaleTakeover(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// job-a ran, then died without releasing (crash / reboot).
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("first acquire failed")
	}
	// job-a is dead: job-b takes the marker over.
	holder, ok, err := s.AcquireRunning(ctx, "job-b", aliveByID(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || holder != "job-b" {
		t.Fatalf("stale takeover: want (job-b, true), got (%q, %v)", holder, ok)
	}
	if got, _ := s.RunningHolder(ctx); got != "job-b" {
		t.Fatalf("marker after takeover: %q", got)
	}
}

func TestAcquireRunningNoTakeoverWhenAlive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("first acquire failed")
	}
	// Same marker, but this time job-a IS still running: job-b must wait.
	holder, ok, err := s.AcquireRunning(ctx, "job-b", aliveByID(map[string]bool{"job-a": true}))
	if err != nil {
		t.Fatal(err)
	}
	if ok || holder != "job-a" {
		t.Fatalf("want held by job-a, got (%q, %v)", holder, ok)
	}
}

func TestAcquireRunningNilAliveNeverTakesOver(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("first acquire failed")
	}
	// No liveness check at all: the marker is simply held.
	holder, ok, err := s.AcquireRunning(ctx, "job-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok || holder != "job-a" {
		t.Fatalf("want held by job-a, got (%q, %v)", holder, ok)
	}
}

func TestReleaseRunningByHolder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("acquire failed")
	}
	if err := s.ReleaseRunning(ctx, "job-a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RunningHolder(ctx); got != "" {
		t.Fatalf("marker after release: %q", got)
	}
}

func TestReleaseRunningByNonHolderIsNoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("acquire failed")
	}
	// A slow round releasing someone else's marker must be a no-op.
	if err := s.ReleaseRunning(ctx, "job-b"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RunningHolder(ctx); got != "job-a" {
		t.Fatalf("marker clobbered: %q", got)
	}
	// And releasing a missing key is not an error.
	if err := s.ReleaseRunning(ctx, "job-b"); err != nil {
		t.Fatal(err)
	}
}

func TestRunningHolderMissingKey(t *testing.T) {
	s := newTestStore(t)
	if got, err := s.RunningHolder(context.Background()); err != nil || got != "" {
		t.Fatalf("want (\"\", nil), got (%q, %v)", got, err)
	}
}

func TestClearRunning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, _ := s.AcquireRunning(ctx, "job-a", nil); !ok {
		t.Fatal("acquire failed")
	}
	if err := s.ClearRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RunningHolder(ctx); got != "" {
		t.Fatalf("marker after clear: %q", got)
	}
	// Clearing an empty key is a no-op.
	if err := s.ClearRunning(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPauseTurnsBothSwitchesOff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if on, err := s.AutoModeOn(ctx); err != nil || on {
		t.Fatalf("pause left auto_mode on: %v, %v", on, err)
	}
	if on, err := s.FullAutoOn(ctx); err != nil || on {
		t.Fatalf("pause left full_auto on: %v, %v", on, err)
	}
}
