package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// setRunningJob claims a job and puts it in the running state the way
// Runner.Run does (the claimed state is carried over, so RanWith survives),
// so yield has a running round to park.
func setRunningJob(t *testing.T, s *agentjob.Store, id string) {
	t.Helper()
	j, err := s.Claim(id, "r1", "local")
	if err != nil {
		t.Fatal(err)
	}
	j.Status = agentjob.Running
	if err := s.SetStatus(id, j.State); err != nil {
		t.Fatal(err)
	}
}

// TestYieldRefusesEmptySections is acceptance (1): a yield whose "In
// flight" / "Next steps" sections come up empty (no flag, no file text)
// is refused — the status stays running and no continuation.json is
// written; the scaffolded .md is left on disk to be filled in.
func TestYieldRefusesEmptySections(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "yy1", agentjob.Ready)
	setRunningJob(t, s, "yy1")

	err = cmdYield(s, []string{"yy1", "--wait-on", "c1", "--note", "note"})
	if err == nil {
		t.Fatal("yield with empty sections: want refusal")
	}
	if !strings.Contains(err.Error(), "yield refused") {
		t.Errorf("error = %v, want a refusal", err)
	}
	if j, _ := s.Get("yy1"); j.Status != agentjob.Running {
		t.Errorf("status = %q, want running — a refused yield flips nothing", j.Status)
	}
	if _, err := os.Stat(s.ContinuationJSONPath("yy1")); !os.IsNotExist(err) {
		t.Errorf("refused yield left continuation.json behind")
	}
	if _, err := os.Stat(s.ContinuationMDPath("yy1")); err != nil {
		t.Errorf("scaffolded continuation.md should stay for the retry: %v", err)
	}
}

// TestYieldWritesFilesAndWaits is acceptance (2): a filled yield writes
// both files and flips the job to waiting with its wait fields.
func TestYieldWritesFilesAndWaits(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "yy2", agentjob.Ready)
	setRunningJob(t, s, "yy2")

	err = cmdYield(s, []string{"yy2", "--wait-on", "c1,c2", "--note", "images are rendering",
		"--in-flight", "- c1: bench render", "--next", "- verify both images"})
	if err != nil {
		t.Fatalf("yield: %v", err)
	}
	j, _ := s.Get("yy2")
	if j.Status != agentjob.Waiting {
		t.Fatalf("status = %q, want waiting", j.Status)
	}
	if len(j.WaitOn) != 2 || j.WaitOn[0] != "c1" || j.WaitOn[1] != "c2" {
		t.Errorf("wait_on = %v", j.WaitOn)
	}
	if j.WaitUntil == nil || !j.WaitUntil.After(time.Now()) {
		t.Errorf("wait_until = %v, want a future deadline", j.WaitUntil)
	}

	md, err := os.ReadFile(s.ContinuationMDPath("yy2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Continuation — yy2", "T yy2",
		"images are rendering", "- c1: bench render", "- verify both images"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("continuation.md missing %q:\n%s", want, md)
		}
	}
	c, err := s.ReadContinuation("yy2")
	if err != nil {
		t.Fatal(err)
	}
	if c.JobID != "yy2" || len(c.WaitOn) != 2 || c.RoundCount != 1 ||
		c.Note != "images are rendering" || c.LastDriver != "local" {
		t.Errorf("continuation = %+v", c)
	}
}

// TestYieldMergesExistingContinuation: a round that wrote the
// continuation.md in its own words keeps that text — the flags override
// only the sections they name, the file fills the rest.
func TestYieldMergesExistingContinuation(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "yy3", agentjob.Ready)
	setRunningJob(t, s, "yy3")
	byHand := "# Continuation — yy3\n\n## Goal\nthe round's own goal\n\n## In flight\nc1 — its own words\n\n## Next steps\n\n"
	if err := os.WriteFile(s.ContinuationMDPath("yy3"), []byte(byHand), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdYield(s, []string{"yy3", "--wait-on", "c1", "--note", "n", "--next", "- the step"}); err != nil {
		t.Fatalf("yield: %v", err)
	}
	md, err := os.ReadFile(s.ContinuationMDPath("yy3"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "c1 — its own words") {
		t.Errorf("hand-written In flight text was lost:\n%s", md)
	}
	if !strings.Contains(string(md), "- the step") {
		t.Errorf("--next did not fill the empty section:\n%s", md)
	}
	if j, _ := s.Get("yy3"); j.Status != agentjob.Waiting {
		t.Errorf("status = %q, want waiting", j.Status)
	}
}

// TestWaitingListsWaitingJobs is acceptance (3a): the waiting list shows
// the waiting job with what it waits on and its deadline — and nothing
// else; an empty store prints nothing.
func TestWaitingListsWaitingJobs(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "w-ready", agentjob.Ready)
	seedJobs(t, s, "w-wait", agentjob.Ready)
	setRunningJob(t, s, "w-wait")
	if err := cmdYield(s, []string{"w-wait", "--wait-on", "a,b", "--note", "n",
		"--in-flight", "- a", "--next", "- x"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdWaiting(s, &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"w-wait", "waiting on a, b", "deadline"} {
		if !strings.Contains(got, want) {
			t.Errorf("waiting output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "w-ready") {
		t.Errorf("waiting output leaked a ready job:\n%s", got)
	}

	s2, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var none bytes.Buffer
	if err := cmdWaiting(s2, &none); err != nil {
		t.Fatal(err)
	}
	if none.String() != "" {
		t.Errorf("empty store should print nothing, got:\n%s", none.String())
	}
}

// TestWakeManual is acceptance (3b/4): a manual wake before the result is
// in keeps the job waiting and says what is missing; after the result it
// flips it back to ready with a priority above the ready peers; a second
// wake of a non-waiting job is an error.
func TestWakeManual(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "wk1", agentjob.Ready)
	seedJobs(t, s, "wk2", agentjob.Ready)
	if err := s.SetOrder("wk2", agentjob.ClassBuild, 50); err != nil {
		t.Fatal(err)
	}
	setRunningJob(t, s, "wk1")
	if err := cmdYield(s, []string{"wk1", "--wait-on", "x", "--note", "n",
		"--in-flight", "- x", "--next", "- y"}); err != nil {
		t.Fatal(err)
	}

	err = cmdWake(s, []string{"wk1"})
	if err == nil {
		t.Fatal("wake before the result is in: want refusal")
	}
	if !strings.Contains(err.Error(), "still waiting") || !strings.Contains(err.Error(), "requeue") {
		t.Errorf("error = %v, want the missing result + a requeue hint", err)
	}
	if j, _ := s.Get("wk1"); j.Status != agentjob.Waiting {
		t.Errorf("status = %q, want still waiting", j.Status)
	}

	if err := s.RecordResult("wk1", "x", "ref-x"); err != nil {
		t.Fatal(err)
	}
	if err := cmdWake(s, []string{"wk1"}); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Get("wk1")
	if j.Status != agentjob.Ready {
		t.Fatalf("status = %q, want ready", j.Status)
	}
	if j.Priority <= 50 {
		t.Errorf("priority = %d, want above the ready peers (50)", j.Priority)
	}

	if err := cmdWake(s, []string{"wk1"}); err == nil {
		t.Error("wake of a ready job: want an error")
	}
}

// TestYieldWakeUsage: the argument checks fire before anything is touched.
func TestYieldWakeUsage(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdYield(s, []string{"no-id"}); err == nil {
		t.Error("yield without --wait-on/--note: want a usage error")
	}
	if err := cmdWake(s, nil); err == nil {
		t.Error("wake without an id: want a usage error")
	}
}
