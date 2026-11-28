// Next / NextLoop picker tests (card ELR-112, M4.4b): agent-run never sees
// a job whose driver is loop; loopd has its own picker.
package agentjob

import (
	"testing"
)

// TestNextSkipsLoopDriverJobs: a ready loop job ahead of a ready local job
// is invisible to Next / NextExcluding — loopd owns those jobs through
// NextLoop, and agent-run must never see one. The loop job stays ready
// (skipped, not consumed).
func TestNextSkipsLoopDriverJobs(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "loopjob", Driver: DriverLoop, Priority: 100})
	ingest(t, s, Spec{ID: "localjob", Driver: "local", Priority: 1})

	j, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("Next returned %+v, want localjob (the loop job is invisible)", j)
	}
	j, err = s.NextExcluding(nil)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("NextExcluding returned %+v, want localjob (the loop job is invisible)", j)
	}
	lj, err := s.Get("loopjob")
	if err != nil {
		t.Fatal(err)
	}
	if lj.Status != Ready {
		t.Fatalf("loop job must stay ready when skipped, got %s", lj.Status)
	}
}

// TestNextLoopReturnsOnlyLoopJobs: NextLoop offers a ready loop job even
// when a higher-priority non-loop job is ahead of it, and returns nil
// when no loop job is ready — it never offers a non-loop job.
func TestNextLoopReturnsOnlyLoopJobs(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "localjob", Driver: "local", Priority: 100})
	ingest(t, s, Spec{ID: "loopjob", Driver: DriverLoop, Priority: 1})

	j, err := s.NextLoop()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "loopjob" {
		t.Fatalf("NextLoop returned %+v, want loopjob (local jobs are invisible to it)", j)
	}

	// Only non-loop jobs ready: nothing for the loop.
	s.SetStatus("loopjob", State{Status: Done})
	j, err = s.NextLoop()
	if err != nil {
		t.Fatal(err)
	}
	if j != nil {
		t.Fatalf("NextLoop with no ready loop jobs returned %+v, want nil", j)
	}
}

// TestNextLoopRespectsDependsOn: a loop job whose dependency is not done is
// not offered; once the dep is done it is.
func TestNextLoopRespectsDependsOn(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "dep"})
	ingest(t, s, Spec{ID: "loopjob", Driver: DriverLoop, DependsOn: []string{"dep"}})

	j, err := s.NextLoop()
	if err != nil {
		t.Fatal(err)
	}
	if j != nil {
		t.Fatalf("NextLoop offered a job whose dep is not done: %+v", j)
	}

	s.SetStatus("dep", State{Status: Done})
	j, err = s.NextLoop()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "loopjob" {
		t.Fatalf("NextLoop returned %+v, want loopjob (its dep is done)", j)
	}
}

// TestNextLoopSkipsClaimed: a loop job loopd already claimed (runtime lock
// held, so the claim is live) is not offered again; the next ready loop
// job is.
func TestNextLoopSkipsClaimed(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Driver: DriverLoop, Priority: 10})
	ingest(t, s, Spec{ID: "b", Driver: DriverLoop, Priority: 1})
	if _, err := s.ClaimLoop("a", "r"); err != nil {
		t.Fatal(err)
	}
	holdLoopLock(t, s) // loopd is alive: a's claim is live, not dead

	j, err := s.NextLoop()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "b" {
		t.Fatalf("NextLoop returned %+v, want b (claimed a is skipped)", j)
	}
}
