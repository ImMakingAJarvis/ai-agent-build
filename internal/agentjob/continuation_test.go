package agentjob

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRenderParseContinuationRoundTrip: a rendered continuation parses back
// with every section intact, and an empty scaffold renders all six headers
// and parses to nothing.
func TestRenderParseContinuationRoundTrip(t *testing.T) {
	content := map[string]string{
		SectionGoal:    "ship the photos",
		SectionDone:    "- queued 3 image jobs",
		SectionFlight:  "- c1: bench render\n- c2: art render",
		SectionResults: "- c1: verify 16x9",
		SectionNext:    "- read results, verify, finish",
		SectionGotchas: "- bench needs 12GB VRAM",
	}
	md := RenderContinuationMD("job1", content)
	if !strings.HasPrefix(md, "# Continuation — job1\n") {
		t.Errorf("rendered header = %q", md[:30])
	}
	got := ParseContinuationMD(md)
	for k, v := range content {
		if got[k] != v {
			t.Errorf("section %q = %q, want %q:\n%s", k, got[k], v, md)
		}
	}

	empty := RenderContinuationMD("job2", map[string]string{})
	for _, name := range []string{SectionGoal, SectionDone, SectionFlight,
		SectionResults, SectionNext, SectionGotchas} {
		if !strings.Contains(empty, "## "+name) {
			t.Errorf("scaffold missing section %q:\n%s", name, empty)
		}
	}
	if got := ParseContinuationMD(empty); len(got) != 0 {
		t.Errorf("empty scaffold parsed = %v, want empty", got)
	}
}

// TestContinuationMDFilled is the invariant-8 check: yield's refusal keys
// off "In flight" and "Next steps" carrying real content, and header
// matching is by prefix so a hand-written shorter header still counts.
func TestContinuationMDFilled(t *testing.T) {
	if in, next := ContinuationMDFilled(RenderContinuationMD("x", map[string]string{})); in || next {
		t.Errorf("empty scaffold: in_flight=%v next=%v, want both false", in, next)
	}
	if in, next := ContinuationMDFilled(RenderContinuationMD("x", map[string]string{SectionNext: "- the step"})); in || !next {
		t.Errorf("partial: in_flight=%v next=%v, want false/true", in, next)
	}
	if in, next := ContinuationMDFilled(RenderContinuationMD("x", map[string]string{
		SectionFlight: "- c1: render", SectionNext: "- verify",
	})); !in || !next {
		t.Errorf("filled: in_flight=%v next=%v, want both true", in, next)
	}
	byHand := "## Goal\ng\n## In flight\nc1\n## Next steps\nstep\n"
	if in, next := ContinuationMDFilled(byHand); !in || !next {
		t.Errorf("hand-written shorter headers: in_flight=%v next=%v, want both true", in, next)
	}
}

// TestContinuationStoreRoundTrip: the continuation.json sidecar — absent
// until a job yields, a faithful round trip after WriteContinuation.
func TestContinuationStoreRoundTrip(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "c1", Title: "cont", Priority: 1, CreatedAt: time.Now().UTC()})

	if _, err := s.ReadContinuation("c1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("read absent continuation: err=%v, want os.ErrNotExist", err)
	}
	c := &Continuation{
		JobID: "c1", Goal: "cont", WaitOn: []string{"a", "b"},
		WaitUntil: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		RoundCount: 1, LastDriver: "local", Note: "n",
		At: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.WriteContinuation(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadContinuation("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "c1" || len(got.WaitOn) != 2 || got.WaitOn[1] != "b" ||
		got.RoundCount != 1 || got.LastDriver != "local" || got.Note != "n" {
		t.Errorf("round trip = %+v", got)
	}
	if _, err := os.Stat(s.ContinuationJSONPath("c1")); err != nil {
		t.Errorf("continuation.json on disk: %v", err)
	}
}

// TestRecordResultPatchesContinuation: without a continuation, RecordResult
// records the result and creates nothing; with one, it keeps the
// continuation's ResultRefs in step, and MissingResults tracks the gap.
func TestRecordResultPatchesContinuation(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "pc1", Title: "patch", Priority: 1, CreatedAt: time.Now().UTC()})
	setRunning(t, s, "pc1")
	if err := s.Yield("pc1", []string{"a", "b"}, time.Now().UTC().Add(time.Hour), "n"); err != nil {
		t.Fatal(err)
	}

	// No continuation yet: the record is plain, no sidecar appears.
	if err := s.RecordResult("pc1", "a", "ref-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadContinuation("pc1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RecordResult created a continuation: %v", err)
	}
	if m, err := s.MissingResults("pc1"); err != nil || len(m) != 1 || m[0] != "b" {
		t.Errorf("missing = %v err=%v, want [b]", m, err)
	}

	// With a continuation (seeded the way cmdYield seeds it — from the
	// results already on disk): the refs follow the results.
	c := &Continuation{JobID: "pc1", WaitOn: []string{"a", "b"}, RoundCount: 1,
		ResultRefs: map[string]string{"a": "ref-a"},
		At: time.Now().UTC().Format(time.RFC3339)}
	if err := s.WriteContinuation(c); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult("pc1", "b", "ref-b"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadContinuation("pc1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResultRefs["a"] != "ref-a" || got.ResultRefs["b"] != "ref-b" {
		t.Errorf("result_refs = %v, want both refs", got.ResultRefs)
	}
	if m, err := s.MissingResults("pc1"); err != nil || len(m) != 0 {
		t.Errorf("missing = %v err=%v, want none", m, err)
	}
}

// TestWaitAgeAndRemaining: the two renderers the waiting list and the
// dashboard share — the same duration buckets, plus the degenerate inputs.
func TestWaitAgeAndRemaining(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	for _, c := range []struct{ since time.Duration; want string }{
		{-45 * time.Second, "45s"},
		{-12 * time.Minute, "12m"},
		{-(2 * time.Hour + 3 * time.Minute), "2h3m"},
		{-(50 * time.Hour), "2d2h"},
	} {
		if got := WaitAge(Job{State: State{WaitingSince: rfc(c.since)}}, now); got != c.want {
			t.Errorf("WaitAge(%v) = %q, want %q", c.since, got, c.want)
		}
	}
	if got := WaitAge(Job{}, now); got != "?" {
		t.Errorf("WaitAge(no waiting_since) = %q, want ?", got)
	}
	if got := WaitAge(Job{State: State{WaitingSince: "garbage"}}, now); got != "?" {
		t.Errorf("WaitAge(bad waiting_since) = %q, want ?", got)
	}

	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	for _, c := range []struct {
		until time.Duration
		want  string
	}{
		{45 * time.Minute, "in 45m"},
		{2 * time.Hour + 3 * time.Minute, "in 2h3m"},
		{-5 * time.Minute, "OVERDUE by 5m"},
	} {
		if got := WaitRemaining(Job{Spec: Spec{WaitUntil: at(c.until)}}, now); got != c.want {
			t.Errorf("WaitRemaining(%v away) = %q, want %q", c.until, got, c.want)
		}
	}
	if got := WaitRemaining(Job{}, now); got != "no deadline" {
		t.Errorf("WaitRemaining(no deadline) = %q", got)
	}
}
