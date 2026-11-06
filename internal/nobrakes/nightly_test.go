package nobrakes

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

var testNow = time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)

func newTestJobs(t *testing.T) *agentjob.Store {
	t.Helper()
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addParkedNightly(t *testing.T, s *agentjob.Store, id, title string, priority int) {
	t.Helper()
	sp := agentjob.Spec{ID: id, Title: title, Tags: []string{agentjob.TagNightly},
		Priority: priority, CreatedAt: time.Now().UTC()}
	if added, err := s.Ingest(sp); err != nil || !added {
		t.Fatalf("ingest %s: added=%v err=%v", id, added, err)
	}
}

func addParkedLinked(t *testing.T, s *agentjob.Store, id, title, kanbanTaskID string, tags []string, priority int) {
	t.Helper()
	sp := agentjob.Spec{ID: id, Title: title, KanbanTaskID: kanbanTaskID, Tags: tags,
		Priority: priority, CreatedAt: time.Now().UTC()}
	if added, err := s.Ingest(sp); err != nil || !added {
		t.Fatalf("ingest %s: added=%v err=%v", id, added, err)
	}
}

func jobStatus(t *testing.T, s *agentjob.Store, id string) string {
	t.Helper()
	j, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Status
}

// TestAutoEnqueueOffIsNoOp: with the flag off (or missing) nothing happens —
// no enqueue, no flag write. This is acceptance criterion 1.
func TestAutoEnqueueOffIsNoOp(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "nightly", 1)

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, nil)
	if err != nil || on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue off: enqueued=%v on=%v err=%v, want all empty", enqueued, on, err)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Parked {
		t.Fatalf("job status after off check: %s, want parked", st)
	}
	if v, err := flags.GetNightlyFlag(ctx); err != nil || v != "" {
		t.Fatalf("nightly flag after off check: %q, %v; want untouched", v, err)
	}
}

// TestAutoEnqueueOnAndDue: flag on + fresh window enqueues every parked
// nightly job in queue order and stamps the window; the same day the check
// is a no-op (idempotent, no double-enqueue). This is acceptance criterion 2.
func TestAutoEnqueueOnAndDue(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "low", 1)
	addParkedNightly(t, jobs, "n2", "high", 5)
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, nil)
	if err != nil || !on {
		t.Fatalf("AutoEnqueue on+due: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	if len(enqueued) != 2 || enqueued[0] != "n2" || enqueued[1] != "n1" {
		t.Fatalf("enqueued = %v, want [n2 n1] (queue order)", enqueued)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Ready {
		t.Errorf("n1 = %s, want ready", st)
	}
	if st := jobStatus(t, jobs, "n2"); st != agentjob.Ready {
		t.Errorf("n2 = %s, want ready", st)
	}
	if v, err := flags.GetNightlyFlag(ctx); err != nil || v != "2026-11-03" {
		t.Fatalf("nightly flag = %q, %v; want 2026-11-03", v, err)
	}

	// Same day, hours later: the window is stamped, nothing re-enqueues.
	again, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow.Add(2*time.Hour), nil)
	if err != nil || !on || len(again) != 0 {
		t.Fatalf("second check same day: enqueued=%v on=%v err=%v, want empty", again, on, err)
	}
}

// TestAutoEnqueueOnNotDue: flag on but the window already processed today —
// parked jobs stay parked.
func TestAutoEnqueueOnNotDue(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "nightly", 1)
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := flags.SetNightlyFlag(ctx, "2026-11-03"); err != nil {
		t.Fatal(err)
	}
	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, nil)
	if err != nil || !on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue not-due: enqueued=%v on=%v err=%v, want empty", enqueued, on, err)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Parked {
		t.Fatalf("job status after not-due check: %s, want parked", st)
	}
	// Tomorrow the window is due again.
	enqueued, _, err = AutoEnqueue(ctx, flags, jobs, nil, testNow.Add(24*time.Hour), nil)
	if err != nil || len(enqueued) != 1 {
		t.Fatalf("next day: enqueued=%v err=%v, want [n1]", enqueued, err)
	}
	if v, _ := flags.GetNightlyFlag(ctx); v != "2026-11-04" {
		t.Fatalf("flag after next day = %q, want 2026-11-04", v)
	}
}

// TestAutoEnqueueOnDueZeroParked: an empty queue still stamps the window —
// the window has been processed; jobs added after it wait for the next one.
func TestAutoEnqueueOnDueZeroParked(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, nil)
	if err != nil || !on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue zero parked: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	if v, err := flags.GetNightlyFlag(ctx); err != nil || v != "2026-11-03" {
		t.Fatalf("flag = %q, %v; want stamped even with nothing parked", v, err)
	}
}

// TestAutoEnqueueRedisDown: a dead Redis is an error the caller turns into
// "off for this iteration" — and nothing is touched. This is acceptance
// criterion 3 (the cmdNightly wrapper turns this into a warning + exit 0).
func TestAutoEnqueueRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "nightly", 1)
	flags := NewFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	if err := flags.SetAutoMode(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	mr.Close() // the server dies; the flag client is now pointing at nothing

	enqueued, on, err := AutoEnqueue(context.Background(), flags, jobs, nil, testNow, nil)
	if err == nil {
		t.Fatal("AutoEnqueue with redis down: want error")
	}
	if on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue with redis down: enqueued=%v on=%v, want empty", enqueued, on)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Parked {
		t.Fatalf("job touched while redis was down: %s", st)
	}
}

// TestNightlyEligibleKanbanTagIsCanonical is NoBrakes 4/6 acceptance
// criterion 2: for parked jobs linked to kanban tasks, the task's nightly
// tag on the board is the canonical auto-eligibility source. A linked job
// with the legacy job tag but an untagged board task is NOT eligible (the
// board wins); a linked job whose task is tagged is; jobs whose link is
// unresolvable — and unlinked jobs — fall back to the job tag.
func TestNightlyEligibleKanbanTagIsCanonical(t *testing.T) {
	jobs := newTestJobs(t)
	board, err := kanban.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := board.AddEpic("epic", "")
	tagged := board.AddTask(ep.ID, "tagged", "", 5)
	if err := board.UpdateTaskNightly(tagged.ID, true); err != nil {
		t.Fatal(err)
	}
	untagged := board.AddTask(ep.ID, "untagged", "", 5)
	ghost := board.AddTask(ep.ID, "ghost", "", 5)
	if err := board.DeleteTask(ghost.ID); err != nil {
		t.Fatal(err)
	}

	// The CLI parks linked jobs of nightly tasks by carrying the job tag
	// (jarvis-jobs add -kanban-task), so k1/k2 both have it.
	addParkedLinked(t, jobs, "k1", "linked+tagged", tagged.ID, []string{agentjob.TagNightly}, 5)
	addParkedLinked(t, jobs, "k2", "linked+untagged", untagged.ID, []string{agentjob.TagNightly}, 4)
	addParkedLinked(t, jobs, "k3", "linked+deleted-task", ghost.ID, []string{agentjob.TagNightly}, 3)
	addParkedNightly(t, jobs, "k4", "unlinked", 2)

	got, err := NightlyEligible(jobs, board)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range got {
		ids = append(ids, j.ID)
	}
	if len(ids) != 3 || ids[0] != "k1" || ids[1] != "k3" || ids[2] != "k4" {
		t.Fatalf("NightlyEligible = %v, want [k1 k3 k4] (k2 excluded: the board tag is canonical)", ids)
	}
}

// TestAutoEnqueueKanbanBoard is NoBrakes 4/6 end-to-end against tagged test
// tasks: with auto_mode on and the window due, the gate enqueues the job
// linked to the nightly-tagged task and leaves the job linked to the
// untagged task parked.
func TestAutoEnqueueKanbanBoard(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	board, err := kanban.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := board.AddEpic("epic", "")
	yes := board.AddTask(ep.ID, "yes", "", 5)
	if err := board.UpdateTaskNightly(yes.ID, true); err != nil {
		t.Fatal(err)
	}
	no := board.AddTask(ep.ID, "no", "", 5)
	addParkedLinked(t, jobs, "b1", "linked to yes", yes.ID, []string{agentjob.TagNightly}, 5)
	addParkedLinked(t, jobs, "b2", "linked to no", no.ID, []string{agentjob.TagNightly}, 4)
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, board, testNow, nil)
	if err != nil || !on {
		t.Fatalf("AutoEnqueue with board: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	if len(enqueued) != 1 || enqueued[0] != "b1" {
		t.Fatalf("enqueued = %v, want [b1] (b2's task is not nightly)", enqueued)
	}
	if st := jobStatus(t, jobs, "b2"); st != agentjob.Parked {
		t.Fatalf("b2 = %s, want parked (board tag is canonical)", st)
	}
}

// newTestAudit opens a throwaway audit log under t.TempDir.
func newTestAudit(t *testing.T) *AuditLog {
	t.Helper()
	l, err := NewAuditLog(filepath.Join(t.TempDir(), "nobrakes", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// TestAutoEnqueueWritesAudit is NoBrakes 5/6 acceptance criterion 3: every
// auto-enqueued job gets a durable audit entry carrying the pass's
// timestamp, the nightly window, the job id/title and trigger=auto_mode.
// The entries are deterministic (they use the passed now, not the wall
// clock) and the second same-day pass adds none.
func TestAutoEnqueueWritesAudit(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "low", 1)
	addParkedNightly(t, jobs, "n2", "high", 5)
	audit := newTestAudit(t)
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, audit)
	if err != nil || !on {
		t.Fatalf("AutoEnqueue: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	if len(enqueued) != 2 {
		t.Fatalf("enqueued = %v, want 2", enqueued)
	}

	entries, err := RecentAudit(audit.Path(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit has %d entries, want 2 (one per auto-enqueued job)", len(entries))
	}
	wantTS := testNow.UTC().Format(time.RFC3339)
	want := []AuditEntry{
		{Timestamp: wantTS, Window: "2026-11-03", JobID: "n2", Title: "high", Trigger: TriggerAutoMode},
		{Timestamp: wantTS, Window: "2026-11-03", JobID: "n1", Title: "low", Trigger: TriggerAutoMode},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("audit[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}

	// Same day again: the window is stamped, nothing re-enqueues and the
	// audit log grows by nothing.
	again, _, err := AutoEnqueue(ctx, flags, jobs, nil, testNow.Add(time.Hour), audit)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass: enqueued=%v err=%v, want empty", again, err)
	}
	entries, err = RecentAudit(audit.Path(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit has %d entries after same-day pass, want still 2", len(entries))
	}
}

// TestAutoEnqueueOffNoAudit: with the flag off nothing runs, so nothing is
// audited — the log stays empty.
func TestAutoEnqueueOffNoAudit(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "nightly", 1)
	audit := newTestAudit(t)

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, audit)
	if err != nil || on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue off: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	entries, err := RecentAudit(audit.Path(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("audit has %d entries with the flag off, want 0", len(entries))
	}
}

// failingRecorder is an AuditRecorder whose Record always errors — it
// stands in for an unwritable audit log.
type failingRecorder struct{ calls int }

func (f *failingRecorder) Record(AuditEntry) error {
	f.calls++
	return errors.New("audit disk full")
}

// TestAutoEnqueueAuditFailureDoesNotBlock: a failed audit write must never
// stop the batch — the jobs are enqueued and the window stamped, and the
// shortfall is reported. This keeps the audit log from being a reason a
// nightly run goes dark.
func TestAutoEnqueueAuditFailureDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	flags := newTestStore(t)
	jobs := newTestJobs(t)
	addParkedNightly(t, jobs, "n1", "nightly", 1)
	rec := &failingRecorder{}
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, nil, testNow, rec)
	if err == nil {
		t.Fatal("want an error reporting the audit shortfall")
	}
	if !on || len(enqueued) != 1 || enqueued[0] != "n1" {
		t.Fatalf("batch blocked by audit failure: enqueued=%v on=%v err=%v", enqueued, on, err)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Ready {
		t.Fatalf("n1 = %s, want ready (the job runs even if its audit write fails)", st)
	}
	// The window is still stamped, so the failure does not trigger a
	// same-day retry storm.
	if v, _ := flags.GetNightlyFlag(ctx); v != "2026-11-03" {
		t.Fatalf("nightly flag = %q, want 2026-11-03", v)
	}
	if rec.calls != 1 {
		t.Fatalf("Record called %d times, want 1", rec.calls)
	}
}
