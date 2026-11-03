package nobrakes

import (
	"context"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
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

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, testNow)
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

	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, testNow)
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
	again, on, err := AutoEnqueue(ctx, flags, jobs, testNow.Add(2*time.Hour))
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
	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, testNow)
	if err != nil || !on || len(enqueued) != 0 {
		t.Fatalf("AutoEnqueue not-due: enqueued=%v on=%v err=%v, want empty", enqueued, on, err)
	}
	if st := jobStatus(t, jobs, "n1"); st != agentjob.Parked {
		t.Fatalf("job status after not-due check: %s, want parked", st)
	}
	// Tomorrow the window is due again.
	enqueued, _, err = AutoEnqueue(ctx, flags, jobs, testNow.Add(24*time.Hour))
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
	enqueued, on, err := AutoEnqueue(ctx, flags, jobs, testNow)
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

	enqueued, on, err := AutoEnqueue(context.Background(), flags, jobs, testNow)
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
