package agentjob

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/lock"
)

// pidNobody is larger than PID_MAX (2^22) on Linux: no such process.
const pidNobody = 2147483646

func holdLoopLock(t *testing.T, s *Store) {
	t.Helper()
	lk, err := lock.Acquire(filepath.Join(s.Dir(), "loop.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lk.Release() })
}

func TestTwentyGoroutinesClaimLoopOneWins(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimLoop("a", "r")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one ClaimLoop may win, got %d", wins)
	}
	j, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Claimed || j.RanWith != DriverLoop {
		t.Fatalf("winner not recorded: %+v", j.State)
	}
}

func TestClaimLoopRecordsRanWithLoopAndZeroPID(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	j, err := s.ClaimLoop("a", "round-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Claimed || j.RanWith != DriverLoop || j.Round != "round-1" {
		t.Fatalf("bad state: %+v", j.State)
	}
	if j.PID != 0 {
		t.Fatalf("loop claim must record PID 0, got %d", j.PID)
	}
	if j.ClaimedAt == "" {
		t.Fatal("ClaimedAt not set")
	}
	got, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Claimed || got.RanWith != DriverLoop || got.PID != 0 || got.ClaimedAt == "" {
		t.Fatalf("persisted state wrong: %+v", got.State)
	}
}

func TestClaimLoopRefusesNonReady(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	if _, err := s.Claim("a", "r", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimLoop("a", "r2"); err == nil {
		t.Fatal("ClaimLoop must refuse a claimed job")
	}
	s.SetStatus("a", State{Status: Done})
	if _, err := s.ClaimLoop("a", "r3"); err == nil {
		t.Fatal("ClaimLoop must refuse a done job")
	}
}

func TestLoopClaimNotReclaimedWhileLockHeld(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	if _, err := s.ClaimLoop("a", "r"); err != nil {
		t.Fatal(err)
	}
	holdLoopLock(t, s)
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Claimed {
		t.Fatalf("loop claim with the runtime lock held must stay claimed, got %s", j.Status)
	}
	if _, err := s.ClaimLoop("a", "r2"); err == nil {
		t.Fatal("a second ClaimLoop must refuse while the first owner lives")
	}
}

func TestLoopClaimReclaimedWhenLockFree(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	if _, err := s.ClaimLoop("a", "r"); err != nil {
		t.Fatal(err)
	}
	// loopd died: nothing holds loop.lock.
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Ready {
		t.Fatalf("dead loop claim must be ready, got %s", j.Status)
	}
	if _, err := s.ClaimLoop("a", "r2"); err != nil {
		t.Fatal("the loop must claim the reclaimed job again")
	}
}

func TestLoopClaimIgnoresPID(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	// A PID of a live process does not keep a loop claim alive when the
	// lock is free.
	s.SetStatus("a", State{Status: Claimed, RanWith: DriverLoop, PID: os.Getpid()})
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.Status != Ready {
		t.Fatalf("live PID must not keep a lock-free loop claim: %s", j.Status)
	}
	// A PID of no process does not kill a loop claim while the lock is
	// held.
	s.SetStatus("a", State{Status: Claimed, RanWith: DriverLoop, PID: pidNobody})
	holdLoopLock(t, s)
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.Status != Claimed {
		t.Fatalf("held lock must keep a loop claim alive: %s", j.Status)
	}
}

func TestNonLoopClaimStillUsesPID(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	if _, err := s.Claim("a", "r", "local"); err != nil {
		t.Fatal(err)
	}
	// Owner alive (this process): the claim survives a Recover.
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.Status != Claimed {
		t.Fatalf("alive PID must keep a non-loop claim: %s", j.Status)
	}
	// Owner dead: the claim is reclaimed, lock or no lock.
	s.SetStatus("a", State{Status: Running, RanWith: "local", PID: pidNobody})
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.Status != Ready {
		t.Fatalf("dead PID must reclaim a non-loop claim: %s", j.Status)
	}
}
