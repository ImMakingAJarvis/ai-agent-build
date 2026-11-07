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
