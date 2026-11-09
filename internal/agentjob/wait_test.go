package agentjob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setRunning claims a job and puts it in the running state the way
// Runner.Run does, so Yield has a running round to yield.
func setRunning(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.Claim(id, "r1", "local"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(id, State{Status: Running}); err != nil {
		t.Fatal(err)
	}
}

// jobIDs is the List() twin of the Spec ids() helper above.
func jobIDs(js []Job) []string {
	o := make([]string, 0, len(js))
	for _, j := range js {
		o = append(o, j.ID)
	}
	return o
}

// pastDeadline rewrites the job's WaitUntil to the past — the test-time
// equivalent of the deadline passing while the job waits (Yield itself
// refuses a past deadline, by design).
func pastDeadline(t *testing.T, s *Store, id string) {
	t.Helper()
	j, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	j.WaitUntil = &past
	if err := writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec); err != nil {
		t.Fatal(err)
	}
}

// TestYieldRunningToWaiting covers the Yield transition and its refusals:
// only a running round may yield, and every wait gets a future deadline
// (invariant 2).
func TestYieldRunningToWaiting(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "y1", Title: "yield", Priority: 5, CreatedAt: time.Now().UTC()})

	// A ready job cannot yield — only a round in flight.
	if err := s.Yield("y1", []string{"y1-img-1"}, time.Now().Add(time.Hour), "n"); err == nil {
		t.Fatal("yield ready job: want refusal")
	}

	setRunning(t, s, "y1")

	// No deadline (or a past one): refused — no job waits forever.
	if err := s.Yield("y1", []string{"y1-img-1"}, time.Time{}, "n"); err == nil {
		t.Fatal("yield without deadline: want refusal")
	}
	if err := s.Yield("y1", []string{"y1-img-1"}, time.Now().Add(-time.Minute), "n"); err == nil {
		t.Fatal("yield with past deadline: want refusal")
	}

	dl := time.Now().UTC().Add(2 * time.Hour)
	if err := s.Yield("y1", []string{"y1-img-1", "y1-img-2"}, dl, "waiting on the images"); err != nil {
		t.Fatalf("yield: %v", err)
	}
	j, err := s.Get("y1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Waiting {
		t.Errorf("status = %q, want %q", j.Status, Waiting)
	}
	if len(j.WaitOn) != 2 || j.WaitOn[0] != "y1-img-1" || j.WaitOn[1] != "y1-img-2" {
		t.Errorf("wait_on = %v", j.WaitOn)
	}
	if got := j.WaitDeadline(); !got.Equal(dl) {
		t.Errorf("wait_until = %v, want %v", got, dl)
	}
	if j.WaitNote != "waiting on the images" || j.WaitingSince == "" {
		t.Errorf("wait_note/waiting_since = %q/%q", j.WaitNote, j.WaitingSince)
	}

	// A second yield of the now-waiting job is refused.
	if err := s.Yield("y1", nil, time.Now().Add(time.Hour), ""); err == nil {
		t.Fatal("yield waiting job: want refusal")
	}
}

// TestWakeOnlyWhenAllWaitOnSatisfied is the partial-wait case: one result in
// does not wake the job, the last one does, and the wake record carries
// every result ref.
func TestWakeOnlyWhenAllWaitOnSatisfied(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "w1", Title: "wait", Priority: 10, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "w1")
	if err := s.Yield("w1", []string{"w1-a", "w1-b"}, time.Now().Add(time.Hour), "two images"); err != nil {
		t.Fatal(err)
	}

	// First result in: still waiting, Wake reports it.
	if err := s.RecordResult("w1", "w1-a", "artifacts/w1/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wake("w1", "artifacts/w1/a.png"); err != ErrWaitOnUnmet {
		t.Fatalf("wake with one of two results: err=%v, want ErrWaitOnUnmet", err)
	}
	if j, _ := s.Get("w1"); j.Status != Waiting {
		t.Fatalf("status after partial wake = %q, want waiting", j.Status)
	}

	// Second result in: it wakes, with both refs in the record.
	if err := s.RecordResult("w1", "w1-b", "artifacts/w1/b.png"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Wake("w1", "artifacts/w1/b.png")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if j, _ := s.Get("w1"); j.Status != Ready {
		t.Fatalf("status after full wake = %q, want ready", j.Status)
	}
	if rec.Reason != WakeAll {
		t.Errorf("reason = %q, want %q", rec.Reason, WakeAll)
	}
	if rec.Results["w1-a"] != "artifacts/w1/a.png" || rec.Results["w1-b"] != "artifacts/w1/b.png" {
		t.Errorf("results = %v", rec.Results)
	}
	if rec.Trigger != "artifacts/w1/b.png" {
		t.Errorf("trigger = %q", rec.Trigger)
	}
	// The record is readable from disk for the resumed round.
	stored, err := s.ReadWake("w1")
	if err != nil || stored.Reason != WakeAll || stored.Results["w1-a"] != "artifacts/w1/a.png" {
		t.Errorf("stored wake record = %+v err=%v", stored, err)
	}
}

// TestWakeTimeoutWhenDeadlinePasses covers invariant 2's backstop: the
// deadline passes with results still missing (or partial) and Wake returns
// the job with a timeout record instead of a full-results one.
func TestWakeTimeoutWhenDeadlinePasses(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "t1", Title: "time out", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "t1")
	if err := s.Yield("t1", []string{"t1-a", "t1-b"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}

	// Deadline passes with one result in: wake with timeout, not unmet.
	if err := s.RecordResult("t1", "t1-a", "artifacts/t1/a.png"); err != nil {
		t.Fatal(err)
	}
	pastDeadline(t, s, "t1")
	rec, err := s.Wake("t1", "")
	if err != nil {
		t.Fatalf("timeout wake: %v", err)
	}
	if rec.Reason != WakeTimeout {
		t.Errorf("reason = %q, want %q", rec.Reason, WakeTimeout)
	}
	if len(rec.Results) != 0 {
		t.Errorf("results = %v, want none on a timeout wake", rec.Results)
	}
	if j, _ := s.Get("t1"); j.Status != Ready {
		t.Fatalf("status after timeout wake = %q, want ready", j.Status)
	}
}

// TestWakeAllBeatsTimeout: when every result is in AND the deadline has
// passed, the wake is a full one — the results are usable.
func TestWakeAllBeatsTimeout(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a1", Title: "all wins", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "a1")
	if err := s.Yield("a1", []string{"a1-a"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult("a1", "a1-a", "artifacts/a1/a.png"); err != nil {
		t.Fatal(err)
	}
	pastDeadline(t, s, "a1")
	rec, err := s.Wake("a1", "")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if rec.Reason != WakeAll || rec.Results["a1-a"] != "artifacts/a1/a.png" {
		t.Errorf("record = %+v, want reason all with the ref", rec)
	}
}

// TestDoubleWakeIsNoOp: a second wake (or a wake of a non-waiting job)
// changes nothing and returns the existing record.
func TestDoubleWakeIsNoOp(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "d1", Title: "double", Priority: 4, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "d1")
	if err := s.Yield("d1", nil, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Wake("d1", "trigger-1")
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := s.Get("d1")
	second, err := s.Wake("d1", "trigger-2")
	if err != nil {
		t.Fatalf("double wake: %v", err)
	}
	if second.At != first.At || second.Reason != first.Reason || second.Trigger != first.Trigger {
		t.Errorf("second wake record = %+v, want the first %+v", second, first)
	}
	j2, _ := s.Get("d1")
	if j2.Status != Ready || j2.Priority != j1.Priority {
		t.Errorf("after double wake: status=%q prio=%d, want ready/%d", j2.Status, j2.Priority, j1.Priority)
	}
	// Waking a job that never waited is a no-op with an empty record.
	ingest(t, s, Spec{ID: "d2", Title: "plain", Priority: 1, CreatedAt: time.Now().UTC()})
	if rec, err := s.Wake("d2", ""); err != nil || rec.Reason != "" {
		t.Errorf("wake of a ready job: rec=%+v err=%v, want empty no-op", rec, err)
	}
}

// TestNextNeverReturnsWaiting: waiting jobs sit out of the run order —
// Next hands out the ready work around them, and a queue of only waiting
// jobs yields nothing.
func TestNextNeverReturnsWaiting(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "n1", Title: "first", Priority: 50, CreatedAt: time.Now().UTC()})
	ingest(t, s, Spec{ID: "n2", Title: "second", Priority: 10, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "n1")
	if err := s.Yield("n1", []string{"n1-x"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}

	// The ready job is handed out even though the waiting one has higher
	// priority.
	j, err := s.Next()
	if err != nil || j == nil || j.ID != "n2" {
		t.Fatalf("next = %+v err=%v, want n2", j, err)
	}
	// Claim the ready job: what remains is a queue of ONLY waiting jobs.
	if _, err := s.Claim("n2", "r2", "local"); err != nil {
		t.Fatal(err)
	}
	if j2, err := s.Next(); err != nil || j2 != nil {
		t.Fatalf("next with only waiting jobs = %+v, want nil", j2)
	}

	// The wake brings it back, ahead of new work.
	if err := s.RecordResult("n1", "n1-x", "artifacts/n1/x.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wake("n1", ""); err != nil {
		t.Fatal(err)
	}
	j3, err := s.Next()
	if err != nil || j3 == nil || j3.ID != "n1" {
		t.Fatalf("next after wake = %+v err=%v, want n1", j3, err)
	}
}

// TestWakeBumpsPriority: the woken job resumes before new work — one above
// the highest ready priority.
func TestWakeBumpsPriority(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "p1", Title: "low", Priority: 10, CreatedAt: time.Now().UTC()})
	ingest(t, s, Spec{ID: "p2", Title: "high", Priority: 20, CreatedAt: time.Now().UTC()})
	ingest(t, s, Spec{ID: "p3", Title: "yielder", Priority: 5, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "p3")
	if err := s.Yield("p3", []string{"p3-x"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult("p3", "p3-x", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wake("p3", "r"); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Get("p3")
	if j.Priority != 21 {
		t.Errorf("priority after wake = %d, want 21 (above the max ready)", j.Priority)
	}
	// ...and it really sorts first in run order.
	jobs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 || jobs[0].ID != "p3" {
		t.Errorf("run order head = %v, want p3 first", jobIDs(jobs))
	}
}

// TestWakeEmptyWaitOn: a yield with nothing to wait on (a manual-wake
// yield) is woken by any Wake — the all-satisfied check is vacuous.
func TestWakeEmptyWaitOn(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "m1", Title: "manual", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "m1")
	if err := s.Yield("m1", nil, time.Now().Add(time.Hour), "waiting for a push"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Wake("m1", "")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if rec.Reason != WakeAll {
		t.Errorf("reason = %q, want %q", rec.Reason, WakeAll)
	}
	if j, _ := s.Get("m1"); j.Status != Ready {
		t.Errorf("status = %q, want ready", j.Status)
	}
}

// TestRecordResultPersistsAndReplaces: the sidecar accumulates one entry
// per corr; a redelivered result replaces its ref instead of blocking.
func TestRecordResultPersistsAndReplaces(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "rr1", Title: "results", Priority: 1, CreatedAt: time.Now().UTC()})
	if err := s.RecordResult("rr1", "a", "first-ref"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult("rr1", "a", "second-ref"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult("rr1", "b", "b-ref"); err != nil {
		t.Fatal(err)
	}
	res, err := s.results("rr1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res["a"].Ref != "second-ref" || res["b"].Ref != "b-ref" {
		t.Errorf("results = %v", res)
	}
	// The sidecar is plain JSON on disk (the switchboard writes it).
	if _, err := os.Stat(filepath.Join(s.JobDir("rr1"), "results.json")); err != nil {
		t.Errorf("results.json: %v", err)
	}
	// An unknown job or empty corr is an error, not a write.
	if err := s.RecordResult("nope", "a", "r"); err == nil {
		t.Error("record for missing job: want error")
	}
	if err := s.RecordResult("rr1", "", "r"); err == nil {
		t.Error("record with empty corr: want error")
	}
}

// TestYieldedRoundFinishKeepsWaiting: the round that yielded ends (exit 0
// or crash) and the runner's finish must not overwrite the waiting status
// with done/failed — the job keeps waiting for its results.
func TestYieldedRoundFinishKeepsWaiting(t *testing.T) {
	s := newStore(t)
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	ingest(t, s, Spec{ID: "f1", Title: "finish", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "f1")
	if err := s.Yield("f1", []string{"f1-x"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	r := runner(t, s, `echo done-for-now`)
	if _, err := r.Finish("f1", 0, "round output"); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Get("f1")
	if j.Status != Waiting {
		t.Fatalf("status after finish = %q, want waiting (not done/failed)", j.Status)
	}
	if j.FinishedAt != "" {
		t.Errorf("finished_at = %q, want empty — the job is not finished", j.FinishedAt)
	}
	if j.WaitUntil == nil || len(j.WaitOn) != 1 {
		t.Errorf("wait fields lost by finish: wait_on=%v wait_until=%v", j.WaitOn, j.WaitUntil)
	}

	// A crash AFTER the yield keeps the wait too: the yield is the
	// explicit declaration and the deadline is the backstop.
	ingest(t, s, Spec{ID: "f2", Title: "crash", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "f2")
	if err := s.Yield("f2", []string{"f2-x"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Finish("f2", 1, "boom")
	if j, _ := s.Get("f2"); j.Status != Waiting {
		t.Errorf("status after crashed yield = %q, want waiting", j.Status)
	}
}

// TestStoreMigrationOldFormat: a store written before the waiting fields
// existed reads and behaves the same — old jobs are unaffected (no status
// change), and the new state machine works on top of an old-format job.
func TestStoreMigrationOldFormat(t *testing.T) {
	s := newStore(t)
	oldSpec := `{"id":"old1","title":"old job","instructions":"do it","priority":7,"created_at":"2026-10-01T00:00:00Z","class":"build"}`
	oldState := `{"status":"running","ran_with":"local","pid":0,"started_at":"2026-10-01T01:00:00Z"}`
	dir := s.JobDir("old1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(oldSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldState), 0o644); err != nil {
		t.Fatal(err)
	}

	// It loads, with the new fields absent (not zero-garbled).
	j, err := s.Get("old1")
	if err != nil {
		t.Fatalf("get old job: %v", err)
	}
	if j.Status != Running || j.Priority != 7 || j.WaitOn != nil || j.WaitUntil != nil || j.WaitingSince != "" {
		t.Errorf("old job = %+v, want running/7 with no wait fields", j)
	}

	// ...and the new machine works on it end to end.
	if err := s.Yield("old1", []string{"old1-x"}, time.Now().Add(time.Hour), "n"); err != nil {
		t.Fatalf("yield old job: %v", err)
	}
	if err := s.RecordResult("old1", "old1-x", "r"); err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Wake("old1", "r"); err != nil || rec.Reason != WakeAll {
		t.Fatalf("wake old job: rec=%+v err=%v", rec, err)
	}
	if j, _ := s.Get("old1"); j.Status != Ready {
		t.Errorf("old job after wake = %q, want ready", j.Status)
	}

	// Existing behaviour on old data is unchanged: a claimed job with a
	// dead owner recovers to ready, List sees everything.
	oldState2 := `{"status":"claimed","pid":0}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldState2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("old1"); j.Status != Ready {
		t.Errorf("recovered status = %q, want ready", j.Status)
	}
	if jobs, err := s.List(); err != nil || len(jobs) != 1 || jobs[0].ID != "old1" {
		t.Errorf("list = %d jobs err=%v", len(jobs), err)
	}
}
