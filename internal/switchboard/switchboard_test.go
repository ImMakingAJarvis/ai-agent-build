package switchboard

// Broker-free unit tests for the switchboard's logic: the listener store,
// the arrival decoder, correlation matching, the wake action, the timeout
// monitor, and the two small error/notifier helpers. The end-to-end behaviour
// against a live broker (wake within 5 s, the chaos restart, the timeout
// wake, the metrics endpoint) is in integration_test.go (-tags integration).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	testutil "github.com/prometheus/client_golang/prometheus/testutil"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func discard() *log.Logger { return log.New(io.Discard, "", 0) }

func newJobs(t *testing.T) *agentjob.Store {
	t.Helper()
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open jobs store: %v", err)
	}
	return s
}

// addRunning ingests a job and moves it to running — the state Yield requires.
// It mirrors setRunning in the agentjob wait tests (Claim, then SetStatus).
func addRunning(t *testing.T, s *agentjob.Store, id string) {
	t.Helper()
	if _, err := s.Ingest(agentjob.Spec{ID: id, Title: id}); err != nil {
		t.Fatalf("ingest %s: %v", id, err)
	}
	if _, err := s.Claim(id, "r1", "local"); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	if err := s.SetStatus(id, agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatalf("set running %s: %v", id, err)
	}
}

// yield parks a running job on waitOn with a deadline of now+until.
func yield(t *testing.T, s *agentjob.Store, id string, waitOn []string, until time.Duration) {
	t.Helper()
	if err := s.Yield(id, waitOn, time.Now().UTC().Add(until), "test wait"); err != nil {
		t.Fatalf("yield %s: %v", id, err)
	}
}

// pastWaitUntil rewrites a job's job.json so its WaitUntil is an hour in the
// past. Yield refuses a past deadline, so the deadline is moved after the
// fact — the same trick the agentjob wait tests use.
func pastWaitUntil(t *testing.T, s *agentjob.Store, id string) {
	t.Helper()
	j, err := s.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	j.Spec.WaitUntil = &past
	b, err := json.Marshal(j.Spec)
	if err != nil {
		t.Fatalf("marshal spec %s: %v", id, err)
	}
	if err := os.WriteFile(filepath.Join(s.JobDir(id), "job.json"), b, 0o644); err != nil {
		t.Fatalf("write job.json %s: %v", id, err)
	}
}

func TestListenerStore(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A valid wake listener stores, and CreatedAt is filled in.
	l := Listener{ID: "l1", Queue: "flow.j1", Action: ActionWake, JobID: "j1", Timeout: time.Minute}
	if err := st.Create(l); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := st.Get("l1"); got.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt was not filled in")
	}

	// Overwriting an existing listener is refused.
	if err := st.Create(Listener{ID: "l1", Queue: "flow.j1", Action: ActionWake, JobID: "j1"}); err == nil {
		t.Fatalf("overwriting l1 was not refused")
	}

	// Malformed listeners are rejected before they touch disk.
	for _, c := range []struct {
		name string
		l    Listener
	}{
		{"bad id", Listener{ID: "a/b", Queue: "q", Action: ActionWake, JobID: "j"}},
		{"no queue", Listener{ID: "x", Action: ActionWake, JobID: "j"}},
		{"unknown action", Listener{ID: "x", Queue: "q", Action: "explode", JobID: "j"}},
		{"wake without job", Listener{ID: "x", Queue: "q", Action: ActionWake}},
		{"forward without target", Listener{ID: "x", Queue: "q", Action: ActionForward}},
	} {
		if err := st.Create(c.l); err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
	}

	// A forward and a notify listener store fine.
	if err := st.Create(Listener{ID: "f1", Queue: "flow.j1", Action: ActionForward, TargetQueue: "flow.other"}); err != nil {
		t.Fatalf("create forward: %v", err)
	}
	if err := st.Create(Listener{ID: "n1", Queue: "cap.x", Action: ActionNotify, Text: "hi"}); err != nil {
		t.Fatalf("create notify: %v", err)
	}

	// A missing Get is os.ErrNotExist.
	if _, err := st.Get("nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get missing: want ErrNotExist, got %v", err)
	}

	// Removing an existing listener works; removing it again is not an error.
	if err := st.Remove("n1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := st.Get("n1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get removed: want ErrNotExist, got %v", err)
	}
	if err := st.Remove("n1"); err != nil {
		t.Fatalf("remove missing should not error: %v", err)
	}

	// A corrupt file is skipped, not fatal to the whole poll.
	if err := os.WriteFile(filepath.Join(dir, "zz-corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	all, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list = %d listeners, want 2 (corrupt skipped)", len(all))
	}
	if all[0].ID != "f1" || all[1].ID != "l1" {
		t.Fatalf("list order = [%s %s], want [f1 l1] (queue then id)", all[0].ID, all[1].ID)
	}

	// Queues groups the listeners by their queue.
	q, err := st.Queues()
	if err != nil {
		t.Fatalf("queues: %v", err)
	}
	if len(q["flow.j1"]) != 2 {
		t.Fatalf("flow.j1 has %d listeners, want 2", len(q["flow.j1"]))
	}

	// Persistence: a fresh store on the same dir sees the same listeners.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, err := st2.Get("l1"); err != nil || got.JobID != "j1" {
		t.Fatalf("persisted l1 = %+v (err %v), want JobID j1", got, err)
	}
}

func TestDecodeArrival(t *testing.T) {
	// A result with artifacts: the ref is the joined artifact paths.
	body, err := queue.EncodeResult(queue.Result{CorrelationID: "c1", Status: "ok", Artifacts: []string{"/a", "/b"}})
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	got, err := decodeArrival(body)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.corr != "c1" || got.ref != "/a /b" {
		t.Fatalf("result+artifacts = %+v, want {c1 /a /b}", got)
	}

	// A result with no artifacts but a summary: the ref is the summary.
	body, _ = queue.EncodeResult(queue.Result{CorrelationID: "c2", Status: "ok", Summary: "done the thing"})
	if got, _ := decodeArrival(body); got.corr != "c2" || got.ref != "done the thing" {
		t.Fatalf("result+summary = %+v, want {c2 done the thing}", got)
	}

	// A result with neither: the ref falls back to the status.
	body, _ = queue.EncodeResult(queue.Result{CorrelationID: "c3", Status: "error"})
	if got, _ := decodeArrival(body); got.corr != "c3" || got.ref != "status=error" {
		t.Fatalf("result+status = %+v, want {c3 status=error}", got)
	}

	// A v1/v2 message (no result status): the ref is the message id.
	mbody, _ := json.Marshal(queue.Message{ID: "m1", Type: "coding", CorrelationID: "c4"})
	got, err = decodeArrival(mbody)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if got.corr != "c4" || got.ref != "m1" {
		t.Fatalf("message = %+v, want {c4 m1}", got)
	}

	// A message with no id: the ref is "msg".
	mbody, _ = json.Marshal(queue.Message{Type: "coding"})
	if got, _ := decodeArrival(mbody); got.corr != "" || got.ref != "msg" {
		t.Fatalf("message no-id = %+v, want {\"\" msg}", got)
	}

	// A mailbox envelope: the corr is the question/answer corr and the ref
	// is the subject (the woken round's continuation cites it). It must be
	// recognized as a mailbox, not fall through to the generic Message
	// branch (which would drop the corr — the field is "corr", not
	// "correlation_id").
	mail, _ := queue.EncodeMailbox(queue.MailboxMessage{
		ID: "mb1", Kind: queue.MailKindAnswer, From: "igpu-helper", To: "brain",
		Subject: "the engine is at 61C", Body: "fine", Corr: "q1",
	})
	got, err = decodeArrival(mail)
	if err != nil {
		t.Fatalf("decode mailbox: %v", err)
	}
	if got.corr != "q1" || got.ref != "the engine is at 61C" {
		t.Fatalf("mailbox = %+v, want {q1 the engine is at 61C}", got)
	}

	// A mailbox with no subject: the ref falls back to the id.
	mail, _ = queue.EncodeMailbox(queue.MailboxMessage{ID: "mb2", Kind: queue.MailKindFYI, From: "brain", To: "sonnet"})
	if got, _ := decodeArrival(mail); got.corr != "" || got.ref != "mb2" {
		t.Fatalf("mailbox no-subject = %+v, want {\"\" mb2}", got)
	}

	// A generic Message whose Kind is set ("agent") is NOT a mailbox (no
	// to field): it must still decode as a message, corr intact.
	mbody, _ = json.Marshal(queue.Message{ID: "m5", Kind: queue.KindAgent, Title: "a job", CorrelationID: "c5"})
	if got, _ := decodeArrival(mbody); got.corr != "c5" || got.ref != "m5" {
		t.Fatalf("agent-kind message = %+v, want {c5 m5}", got)
	}

	// An undecodable body is an error (the caller dead-letters it).
	if _, err := decodeArrival([]byte("not json")); err == nil {
		t.Fatalf("invalid body: want an error")
	}
}

func TestAnyMatch(t *testing.T) {
	if anyMatch(nil, "c1") {
		t.Fatalf("no listeners should not match anything")
	}
	if !anyMatch([]Listener{{ID: "a", CorrFilter: ""}}, "whatever") {
		t.Fatalf("an empty filter should claim any correlation")
	}
	ls := []Listener{{ID: "a", CorrFilter: "c1"}, {ID: "b", CorrFilter: "c2"}}
	if !anyMatch(ls, "c1") || !anyMatch(ls, "c2") {
		t.Fatalf("a filter should match its own correlation")
	}
	if anyMatch(ls, "c3") {
		t.Fatalf("c3 is not claimed by any listener")
	}
}

func TestDoWake(t *testing.T) {
	m, _ := NewMetrics()
	jobs := newJobs(t)
	sb := New(Config{Jobs: jobs, M: m, Log: discard()})

	// A single correlation: the one result wakes the job (reason all).
	addRunning(t, jobs, "w1")
	yield(t, jobs, "w1", []string{"c1"}, time.Hour)
	sb.doWake(Listener{ID: "l", Queue: "flow.w1", Action: ActionWake, JobID: "w1"}, "c1", "/artifacts/w1/out.png")
	if j, _ := jobs.Get("w1"); j.Status != agentjob.Ready {
		t.Fatalf("w1 = %s, want ready", j.Status)
	}
	rec, _ := jobs.ReadWake("w1")
	if rec.Reason != agentjob.WakeAll {
		t.Fatalf("w1 wake reason = %s, want all", rec.Reason)
	}
	if rec.Results["c1"] != "/artifacts/w1/out.png" {
		t.Fatalf("w1 result ref = %q", rec.Results["c1"])
	}
	if got := testutil.ToFloat64(m.WakesTotal); got != 1 {
		t.Fatalf("wakes = %v, want 1", got)
	}

	// Waking a job that is no longer waiting is a no-op (no extra count).
	sb.doWake(Listener{ID: "l", Queue: "flow.w1", Action: ActionWake, JobID: "w1"}, "c1", "again")
	if got := testutil.ToFloat64(m.WakesTotal); got != 1 {
		t.Fatalf("a wake of a ready job was counted: %v", got)
	}

	// Two correlations: the first result alone does not wake (still waiting on
	// the second); the second one does.
	addRunning(t, jobs, "w2")
	yield(t, jobs, "w2", []string{"cA", "cB"}, time.Hour)
	sb.doWake(Listener{ID: "l", Queue: "flow.w2", Action: ActionWake, JobID: "w2"}, "cA", "refA")
	if j, _ := jobs.Get("w2"); j.Status != agentjob.Waiting {
		t.Fatalf("w2 after first result = %s, want waiting", j.Status)
	}
	if got := testutil.ToFloat64(m.WakesTotal); got != 1 {
		t.Fatalf("a partial result counted a wake: %v", got)
	}
	sb.doWake(Listener{ID: "l", Queue: "flow.w2", Action: ActionWake, JobID: "w2"}, "cB", "refB")
	if j, _ := jobs.Get("w2"); j.Status != agentjob.Ready {
		t.Fatalf("w2 after second result = %s, want ready", j.Status)
	}
	if got := testutil.ToFloat64(m.WakesTotal); got != 2 {
		t.Fatalf("wakes = %v, want 2", got)
	}
}

func TestCheckTimeouts(t *testing.T) {
	m, _ := NewMetrics()
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	sb := New(Config{Jobs: jobs, Listen: ls, M: m, Log: discard()})

	// tA: future deadline, no result → stays waiting.
	addRunning(t, jobs, "tA")
	yield(t, jobs, "tA", []string{"cA"}, time.Hour)
	if err := ls.Create(Listener{ID: "lA", Queue: "flow.tA", Action: ActionWake, JobID: "tA", Timeout: time.Minute}); err != nil {
		t.Fatalf("create lA: %v", err)
	}

	// tB: past deadline, no result → wakes with reason timeout.
	addRunning(t, jobs, "tB")
	yield(t, jobs, "tB", []string{"cB"}, time.Hour)
	pastWaitUntil(t, jobs, "tB")
	if err := ls.Create(Listener{ID: "lB", Queue: "flow.tB", Action: ActionWake, JobID: "tB", Timeout: time.Minute}); err != nil {
		t.Fatalf("create lB: %v", err)
	}

	// tC: past deadline but its result landed → wakes with reason all, not
	// timeout (a result beat the deadline).
	addRunning(t, jobs, "tC")
	yield(t, jobs, "tC", []string{"cC"}, time.Hour)
	pastWaitUntil(t, jobs, "tC")
	if err := jobs.RecordResult("tC", "cC", "refC"); err != nil {
		t.Fatalf("record tC: %v", err)
	}
	if err := ls.Create(Listener{ID: "lC", Queue: "flow.tC", Action: ActionWake, JobID: "tC", Timeout: time.Minute}); err != nil {
		t.Fatalf("create lC: %v", err)
	}

	sb.checkTimeouts(context.Background())

	if j, _ := jobs.Get("tA"); j.Status != agentjob.Waiting {
		t.Fatalf("tA = %s, want waiting (deadline not passed)", j.Status)
	}
	if j, _ := jobs.Get("tB"); j.Status != agentjob.Ready {
		t.Fatalf("tB = %s, want ready (timed out)", j.Status)
	}
	if rec, _ := jobs.ReadWake("tB"); rec.Reason != agentjob.WakeTimeout {
		t.Fatalf("tB wake reason = %s, want timeout", rec.Reason)
	}
	if j, _ := jobs.Get("tC"); j.Status != agentjob.Ready {
		t.Fatalf("tC = %s, want ready", j.Status)
	}
	if rec, _ := jobs.ReadWake("tC"); rec.Reason != agentjob.WakeAll {
		t.Fatalf("tC wake reason = %s, want all (result landed)", rec.Reason)
	}
	if got := testutil.ToFloat64(m.TimeoutsTotal); got != 1 {
		t.Fatalf("timeouts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WakesTotal); got != 1 {
		t.Fatalf("wakes = %v, want 1 (tC landed a result)", got)
	}
}

func TestNotify(t *testing.T) {
	m, _ := NewMetrics()
	// A nil notifier is treated as success (notify disabled, no retry loop).
	sb := New(Config{M: m, Log: discard()})
	if err := sb.notify(context.Background(), "hi"); err != nil {
		t.Fatalf("nil notifier should succeed, got %v", err)
	}
	// A real notifier is called with the text and its error is surfaced.
	called := false
	sb2 := New(Config{
		M: m,
		Notify: func(ctx context.Context, text string) error {
			called = true
			if text != "hello" {
				t.Errorf("notifier got %q, want hello", text)
			}
			return errors.New("discord down")
		},
		Log: discard(),
	})
	if err := sb2.notify(context.Background(), "hello"); err == nil {
		t.Fatalf("expected the notifier's error to be returned")
	}
	if !called {
		t.Fatalf("the notifier was not called")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(&amqp.Error{Code: int(amqp.NotFound)}) {
		t.Fatalf("a 404 should be recognised as not-found")
	}
	if isNotFound(&amqp.Error{Code: 406}) {
		t.Fatalf("a 406 (precondition-failed) is not not-found")
	}
	if isNotFound(errors.New("plain")) {
		t.Fatalf("a plain error is not not-found")
	}
	// A wrapped 404 is still recognised (errors.As unwraps).
	if !isNotFound(fmt.Errorf("get: %w", &amqp.Error{Code: int(amqp.NotFound)})) {
		t.Fatalf("a wrapped 404 should be not-found")
	}
}
