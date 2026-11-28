package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/lock"
)

func seedWithDriver(t *testing.T, s *agentjob.Store, id, driver string, priority int) {
	t.Helper()
	args := []string{"-id", id, "-title", "T " + id, "-priority", strconv.Itoa(priority)}
	if driver != "" {
		args = append(args, "-driver", driver)
	}
	if err := cmdAdd(s, args, strings.NewReader("do it")); err != nil {
		t.Fatal(err)
	}
}

// TestNextRunnableSkipsDownEngines: a local job ahead of an igpu job is
// skipped (left ready) while the brain is down, so the worker idles through
// a swap window instead of burning the local round.
func TestNextRunnableSkipsDownEngines(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWithDriver(t, s, "localjob", "local", 10)
	seedWithDriver(t, s, "igpujob", "igpu", 1)

	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(brain.Close)
	igpu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(igpu.Close)
	t.Setenv("JARVIS_BRAIN_URL", brain.URL)
	t.Setenv("JARVIS_IGPU_URL", igpu.URL)

	// Both up: the higher-priority local job wins.
	j, err := nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("nextRunnable with both engines up got %v, want localjob", j)
	}

	// The brain dies: localjob is skipped, igpujob runs.
	brain.Close()
	j, err = nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "igpujob" {
		t.Fatalf("nextRunnable with brain down got %v, want igpujob", j)
	}

	// Both engines down: nothing runnable.
	igpu.Close()
	j, err = nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j != nil {
		t.Fatalf("nextRunnable with all engines down got %v, want nil", j)
	}
}

// TestNextRunnableSkipsLocalJobsUnderGameHold: while resourced's
// game-hold marker exists (a game wants the 4090 — brain-out pending,
// brain out, or brain coming back), driver-local jobs are skipped even
// when the brain answers /health — the marker covers the gap the
// engine-down skip does not (the brain is still up while the cut round
// sits back in the ready queue). Non-local drivers are unaffected, and
// the local job is runnable again once the marker is cleared.
func TestNextRunnableSkipsLocalJobsUnderGameHold(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWithDriver(t, s, "localjob", "local", 10)
	seedWithDriver(t, s, "igpujob", "igpu", 1)

	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(brain.Close)
	igpu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(igpu.Close)
	t.Setenv("JARVIS_BRAIN_URL", brain.URL)
	t.Setenv("JARVIS_IGPU_URL", igpu.URL)
	holdFile := filepath.Join(t.TempDir(), "game-hold")
	t.Setenv("JARVIS_GAME_HOLD_FILE", holdFile)

	// No marker: the higher-priority local job wins.
	j, err := nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("no marker: nextRunnable got %v, want localjob", j)
	}

	// The marker is on (a game holds the 4090): localjob is skipped even
	// with the brain up, igpujob runs.
	if err := os.WriteFile(holdFile, []byte("2026-11-18T21:26:46Z"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err = nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "igpujob" {
		t.Fatalf("game-hold marker on: nextRunnable got %v, want igpujob (local stays ready)", j)
	}

	// The brain is back and the marker is cleared: localjob is runnable.
	if err := os.Remove(holdFile); err != nil {
		t.Fatal(err)
	}
	j, err = nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("marker cleared: nextRunnable got %v, want localjob", j)
	}
}

func TestEffectiveDriver(t *testing.T) {
	conf := t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "driver"), []byte("igpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_CONF", conf)

	if got := effectiveDriver(&agentjob.Job{Spec: agentjob.Spec{ID: "a"}}); got != "igpu" {
		t.Fatalf("no spec driver: got %q, want igpu (from conf)", got)
	}
	if got := effectiveDriver(&agentjob.Job{Spec: agentjob.Spec{ID: "a", Driver: "sonnet"}}); got != "sonnet" {
		t.Fatalf("spec driver: got %q, want sonnet", got)
	}
}

func TestRequeueDriverClassPriority(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWithDriver(t, s, "rq1", "local", 5)

	// -driver + -priority: class is kept (cmdAdd defaults to build).
	if err := cmdRequeue(s, []string{"-driver", "igpu", "-priority", "42", "rq1"}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("rq1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Driver != "igpu" || j.Class != agentjob.ClassBuild || j.Priority != 42 {
		t.Fatalf("after requeue: driver=%q class=%q prio=%d, want igpu/build/42", j.Driver, j.Class, j.Priority)
	}
	if j.Status != agentjob.Ready {
		t.Fatalf("status %q, want ready", j.Status)
	}

	// -class only: priority is kept.
	if err := cmdRequeue(s, []string{"-class", "fix", "rq1"}); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Get("rq1")
	if j.Class != agentjob.ClassFix || j.Priority != 42 {
		t.Fatalf("after class requeue: class=%q prio=%d, want fix/42", j.Class, j.Priority)
	}

	if err := cmdRequeue(s, []string{"-class", "nope", "rq1"}); err == nil {
		t.Fatal("requeue accepted a bad class")
	}
	if err := cmdRequeue(s, nil); err == nil {
		t.Fatal("requeue accepted no args")
	}
}

func TestHealthUp(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)
	loading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(loading.Close)
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()

	if !healthUp(ok.URL + "/health") {
		t.Error("200 should count as up")
	}
	if healthUp(loading.URL + "/health") {
		t.Error("503 (model loading) should count as down")
	}
	if healthUp(deadURL + "/health") {
		t.Error("connection refused should count as down")
	}
}

func upServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestJarvisJobsNextNeverPrintsLoopJob: with a ready loop job ahead of a
// ready local job, `next` (nextRunnable) returns the local one — agent-run
// never sees a job whose driver is loop (card ELR-112, M4.4b). The loop
// job stays ready for loopd's own picker.
func TestJarvisJobsNextNeverPrintsLoopJob(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWithDriver(t, s, "loopjob", "loop", 100)
	seedWithDriver(t, s, "localjob", "local", 1)
	t.Setenv("JARVIS_BRAIN_URL", upServer(t))

	j, err := nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("next returned %+v, want localjob (the loop job is invisible to agent-run)", j)
	}
	lj, err := s.Get("loopjob")
	if err != nil {
		t.Fatal(err)
	}
	if lj.Status != agentjob.Ready {
		t.Fatalf("loopjob must stay ready for loopd, got %s", lj.Status)
	}
}

// TestLoopJobClaimedByLiveLoopdNotReclaimedByNext: while loopd holds the
// runtime lock, a loop job it has claimed is neither returned by `next`
// nor requeued by it — the poll leaves the live claim alone
// (card ELR-112, M4.4b).
func TestLoopJobClaimedByLiveLoopdNotReclaimedByNext(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWithDriver(t, s, "loopjob", "loop", 100)
	seedWithDriver(t, s, "localjob", "local", 1)

	if _, err := s.ClaimLoop("loopjob", "r"); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(filepath.Join(s.Dir(), "loop.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lk.Release() })
	t.Setenv("JARVIS_BRAIN_URL", upServer(t))

	j, err := nextRunnable(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "localjob" {
		t.Fatalf("next returned %+v, want localjob (the claimed loopjob is neither offered nor reclaimed)", j)
	}
	lj, err := s.Get("loopjob")
	if err != nil {
		t.Fatal(err)
	}
	if lj.Status != agentjob.Claimed {
		t.Fatalf("loopjob must stay claimed while loopd lives, got %s", lj.Status)
	}
}
