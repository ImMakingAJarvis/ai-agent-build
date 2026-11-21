package agentjob

import (
	"strings"
	"testing"
	"time"
)

// depSpec is a ready job with a fixed creation time, for dependency tests.
func depSpec(id string, deps ...string) Spec {
	return Spec{
		ID:        id,
		Title:     "job " + id,
		Class:     ClassBuild,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		DependsOn: deps,
	}
}

// mustIngest ingests one job and fails the test on error.
func mustIngest(t *testing.T, s *Store, sp Spec) {
	t.Helper()
	added, err := s.Ingest(sp)
	if err != nil || !added {
		t.Fatalf("ingest %s: added=%v err=%v", sp.ID, added, err)
	}
}

// TestSweepDepsParksBlockedReadyJobs is the core of the fix: a ready job
// whose dependency failed, was skipped, or is missing is parked with the
// blocker named, instead of sitting ready forever (the silent plan-to-task
// deadlock) and being counted as runnable work.
func TestSweepDepsParksBlockedReadyJobs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depState string // "", "failed", "skipped" — "" = dep missing
		wantBlock string
	}{
		{"failedDep", "failed", "head:failed"},
		{"skippedDep", "skipped", "head:skipped"},
		{"missingDep", "", "head:missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if tc.depState != "" {
				mustIngest(t, s, depSpec("head"))
				if err := s.SetStatus("head", State{Status: tc.depState}); err != nil {
					t.Fatal(err)
				}
			}
			mustIngest(t, s, depSpec("j1", "head"))

			changed, err := s.SweepDeps()
			if err != nil {
				t.Fatal(err)
			}
			if len(changed) != 1 || changed[0].ID != "j1" {
				t.Fatalf("changed=%v, want just j1", changed)
			}
			j, err := s.Get("j1")
			if err != nil {
				t.Fatal(err)
			}
			if j.Status != Parked || j.DepBlock != tc.wantBlock {
				t.Fatalf("j1 = %s %q, want parked %q", j.Status, j.DepBlock, tc.wantBlock)
			}
			wantSummary := "dep-blocked: head is " + strings.TrimPrefix(tc.wantBlock, "head:")
			if j.Summary != wantSummary {
				t.Fatalf("summary = %q, want %q", j.Summary, wantSummary)
			}
			// The blocked job must not be offered by Next.
			if n, err := s.Next(); err != nil || n != nil {
				t.Fatalf("next = %v, %v; want nil", n, err)
			}
			// A second sweep is a no-op: the queue is already honest.
			changed, err = s.SweepDeps()
			if err != nil {
				t.Fatal(err)
			}
			if len(changed) != 0 {
				t.Fatalf("second sweep changed %v, want none", changed)
			}
		})
	}
}

// TestSweepDepsTransitive parks every link of a chain behind a failed head,
// not only the job directly next to it.
func TestSweepDepsTransitive(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("head"))
	mustIngest(t, s, depSpec("mid", "head"))
	mustIngest(t, s, depSpec("tail", "mid"))
	if err := s.SetStatus("head", State{Status: Failed}); err != nil {
		t.Fatal(err)
	}

	// SetStatus already swept: head failed -> mid parked -> (transitively)
	// tail parked in the same pass sequence.
	for id, want := range map[string]string{"mid": "head:failed", "tail": "mid:dep-blocked"} {
		j, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != Parked || j.DepBlock != want {
			t.Fatalf("%s = %s %q, want parked %q", id, j.Status, j.DepBlock, want)
		}
	}
}

// TestSweepDepsRequeueUnblocks is the other direction: re-queueing the
// failed head returns its (transitive) dependents to ready, waiting in the
// queue for it as any other unmet dep.
func TestSweepDepsRequeueUnblocks(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("head"))
	mustIngest(t, s, depSpec("mid", "head"))
	mustIngest(t, s, depSpec("tail", "mid"))
	if err := s.SetStatus("head", State{Status: Failed}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("head", State{Status: Ready}); err != nil { // requeue
		t.Fatal(err)
	}
	for _, id := range []string{"mid", "tail"} {
		j, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != Ready || j.DepBlock != "" || j.Summary != "" {
			t.Fatalf("%s = %s %q %q, want ready, no block, no summary", id, j.Status, j.DepBlock, j.Summary)
		}
	}
	// head is ready again (not done): it runs next, and mid/tail wait for
	// it in the queue like any other unmet dep.
	if n, err := s.Next(); err != nil || n == nil || n.ID != "head" {
		t.Fatalf("next = %v, %v; want head", n, err)
	}
}

// TestSweepDepsDoneDepUnblocksAndRuns: marking the blocker done both
// unblocks the dependent and makes it immediately runnable.
func TestSweepDepsDoneDepUnblocksAndRuns(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("head"))
	mustIngest(t, s, depSpec("j1", "head"))
	if err := s.SetStatus("head", State{Status: Failed}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("head", State{Status: Done}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Ready || j.DepBlock != "" {
		t.Fatalf("j1 = %s %q, want ready unblocked", j.Status, j.DepBlock)
	}
	n, err := s.Next()
	if err != nil || n == nil || n.ID != "j1" {
		t.Fatalf("next = %v, %v; want j1", n, err)
	}
}

// TestSweepDepsLeavesOtherParkedJobsUntouched: jobs parked for another
// reason (no DepBlock) are never re-queued, and a dependent of such a job
// waits for it in the queue instead of being parked (the job may still run
// — nightly gate, manual stop — so it is a delay, not a block).
func TestSweepDepsLeavesOtherParkedJobsUntouched(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("holder"))
	if err := s.SetStatus("holder", State{Status: Parked}); err != nil { // manual stop
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("j1", "holder"))
	// A waiting dep is also only a delay.
	mustIngest(t, s, depSpec("waiting"))
	if err := s.SetStatus("waiting", State{Status: Waiting}); err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("j2", "waiting"))

	changed, err := s.SweepDeps()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("sweep changed %v, want none", changed)
	}
	for _, id := range []string{"j1", "j2"} {
		j, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != Ready || j.DepBlock != "" {
			t.Fatalf("%s = %s %q, want ready unblocked", id, j.Status, j.DepBlock)
		}
	}
	if j, _ := s.Get("holder"); j.Status != Parked || j.DepBlock != "" {
		t.Fatalf("holder = %s %q, want parked untouched", j.Status, j.DepBlock)
	}
}

// TestSweepDepsNightlyParkedDep is the nightly case: a nightly job is
// ingested parked (no DepBlock); its dependent waits for the gate, it is
// not parked itself, and when the gate enqueues the dep the dependent stays
// ready and simply runs once the dep is done.
func TestSweepDepsNightlyParkedDep(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sp := depSpec("night")
	sp.Tags = []string{TagNightly}
	mustIngest(t, s, sp)
	mustIngest(t, s, depSpec("j1", "night"))
	if _, err := s.SweepDeps(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("j1"); j.Status != Ready || j.DepBlock != "" {
		t.Fatalf("j1 = %s %q, want ready unblocked", j.Status, j.DepBlock)
	}
	// Gate enqueues the nightly dep, then it finishes: j1 unblocks and runs.
	if err := s.SetStatus("night", State{Status: Ready}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("night", State{Status: Done}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Next()
	if err != nil || n == nil || n.ID != "j1" {
		t.Fatalf("next = %v, %v; want j1", n, err)
	}
}

// TestNextExcludingSweeps proves the agent-run poll path: `next` both
// returns the first runnable job and has parked the blocked ones along the
// way, so a queue with 80 dep-blocked jobs and one live job behaves like a
// one-job queue.
func TestNextExcludingSweeps(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, depSpec("dead"))
	if err := s.SetStatus("dead", State{Status: Failed}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := "blocked" + string(rune('a'+i))
		mustIngest(t, s, depSpec(id, "dead"))
	}
	mustIngest(t, s, depSpec("live"))

	n, err := s.Next()
	if err != nil || n == nil || n.ID != "live" {
		t.Fatalf("next = %v, %v; want live", n, err)
	}
	for i := 0; i < 5; i++ {
		id := "blocked" + string(rune('a'+i))
		if j, _ := s.Get(id); j.Status != Parked || j.DepBlock != "dead:failed" {
			t.Fatalf("%s = %s %q, want parked dead:failed", id, j.Status, j.DepBlock)
		}
	}
}
