package agentjob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestFixNowContextRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("JARVIS_REDIS", "redis://"+mr.Addr())

	fc := &FixNowContext{
		JobID:      "job-a",
		Title:      "the interrupted job",
		Driver:     "local",
		Model:      "jarvis-brain",
		Round:      "r1",
		SavedAt:    "2026-11-09T00:00:00Z",
		Transcript: `{"type":"assistant","text":"half done"}`,
	}
	if err := SaveFixNowContext(fc); err != nil {
		t.Fatal(err)
	}
	// The record lives under the job key with the TTL — the "doesn't pile up"
	// guard (an interrupted job never re-run expires after FixNowTTL).
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	if ttl, err := rdb.TTL(context.Background(), fixNowCtxKey("job-a")).Result(); err != nil || ttl <= 0 {
		t.Fatalf("ttl=%v err=%v", ttl, err)
	}

	// The re-run reads the context back (and consumes it).
	got := TakeFixNowContext("job-a")
	if got == nil {
		t.Fatal("TakeFixNowContext returned nil")
	}
	if got.JobID != fc.JobID || got.Title != fc.Title || got.Model != fc.Model || got.Transcript != fc.Transcript {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// Consumed: a second take returns nothing, so the block never re-injects.
	if again := TakeFixNowContext("job-a"); again != nil {
		t.Errorf("second take = %+v, want nil (consumed)", again)
	}
	// A novel job never had a saved context: the always-on check is a no-op.
	if novel := TakeFixNowContext("nope"); novel != nil {
		t.Errorf("novel take = %+v, want nil", novel)
	}
}

func TestFixNowContextRedisDisabled(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	// A dead/disabled Redis must never fail the save or the load.
	if err := SaveFixNowContext(&FixNowContext{JobID: "a", Title: "t"}); err != nil {
		t.Errorf("save with redis off should be a nil no-op, got %v", err)
	}
	if got := TakeFixNowContext("a"); got != nil {
		t.Errorf("take with redis off = %+v, want nil", got)
	}
}

func TestFixNowResumeBlock(t *testing.T) {
	if fixNowResumeBlock(nil) != "" {
		t.Fatal("nil context must render nothing (novel job)")
	}
	fc := &FixNowContext{
		JobID: "job-a", Title: "the interrupted job",
		Driver: "local", Model: "jarvis-brain", Round: "r1",
		SavedAt:    "2026-11-09T00:00:00Z",
		Transcript: "did step 1, was on step 2",
	}
	b := fixNowResumeBlock(fc)
	for _, want := range []string{"RESUME", "!fixnow", "job-a", "the interrupted job", "did step 1, was on step 2", "transcript.preempted.txt"} {
		if !strings.Contains(b, want) {
			t.Errorf("resume block missing %q:\n%s", want, b)
		}
	}
}

func TestFixNowResumeBlockBoundedTail(t *testing.T) {
	// Even with a stored transcript longer than the prompt tail, only the tail
	// is injected — the resume block must stay small.
	fc := &FixNowContext{JobID: "a", Title: "t", Transcript: strings.Repeat("x", fixNowPromptTail+1000)}
	b := fixNowResumeBlock(fc)
	if strings.Contains(b, strings.Repeat("x", fixNowPromptTail+1000)) {
		t.Error("unbounded transcript leaked into the prompt")
	}
}

func TestBoundTranscript(t *testing.T) {
	if got := boundTranscript("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	long := strings.Repeat("y", fixNowCtxMax+500)
	got := boundTranscript(long)
	if len(got) != fixNowCtxMax {
		t.Errorf("len = %d, want %d", len(got), fixNowCtxMax)
	}
	if !strings.HasSuffix(got, strings.Repeat("y", 100)) {
		t.Error("boundTranscript must keep the tail")
	}
}

// TestFixNowPreemptionSavesContext runs a real (long) round, cuts it off with a
// !fixnow restart signal, and checks the killed round saved its context to
// Redis for the re-run.
func TestFixNowPreemptionSavesContext(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("JARVIS_REDIS", "redis://"+mr.Addr())
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	restartFile := filepath.Join(t.TempDir(), "restart-signal")
	r := runner(t, s, `echo starting; while sleep 0.1; do :; done`)
	r.SignalFile = filepath.Join(t.TempDir(), "pause-signal") // starts the watcher
	r.RestartSignalFile = restartFile

	done := make(chan struct{})
	go func() {
		_, _ = r.Run(context.Background(), "a", "r1", "local", "")
		close(done)
	}()

	// Wait until the round is actually running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := s.Get("a")
		if got.Status == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never running: %s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A !fixnow preemption cuts the round off (distinct from a plain !restart,
	// which would save no context).
	_ = os.WriteFile(restartFile, []byte(FixNowSignal), 0o644)
	<-done

	// The killed round saved its context for the re-run.
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	b, err := rdb.Get(context.Background(), fixNowCtxKey("a")).Bytes()
	if err != nil {
		t.Fatalf("fixnow context not saved: %v", err)
	}
	var saved FixNowContext
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.JobID != "a" || saved.Title != "A" {
		t.Errorf("saved = %+v", saved)
	}
	if !strings.Contains(saved.Transcript, "starting") {
		t.Errorf("saved transcript missing the round output: %q", saved.Transcript)
	}
	// The cut-off round's transcript is also written to the dedicated file so
	// the re-run's resume block can point the model at it (transcript.txt is
	// truncated by the re-run and would otherwise hold the new round's output).
	if pf, err := os.ReadFile(filepath.Join(s.JobDir("a"), FixNowPreemptedFile)); err != nil {
		t.Fatalf("preempted transcript not written: %v", err)
	} else if !strings.Contains(string(pf), "starting") {
		t.Errorf("preempted transcript missing the round output: %q", string(pf))
	}
	if r.restartSig() != FixNowSignal {
		t.Errorf("restartSig = %q, want %q", r.restartSig(), FixNowSignal)
	}
}

// TestRestartSignalDoesNotSaveContext is the inverse: a plain !restart cuts the
// round off but saves nothing (it is a deliberate fresh start).
func TestRestartSignalDoesNotSaveContext(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("JARVIS_REDIS", "redis://"+mr.Addr())
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	restartFile := filepath.Join(t.TempDir(), "restart-signal")
	r := runner(t, s, `echo starting; while sleep 0.1; do :; done`)
	r.SignalFile = filepath.Join(t.TempDir(), "pause-signal")
	r.RestartSignalFile = restartFile

	done := make(chan struct{})
	go func() {
		_, _ = r.Run(context.Background(), "a", "r1", "local", "")
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := s.Get("a")
		if got.Status == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never running: %s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = os.WriteFile(restartFile, []byte("!restart"), 0o644)
	<-done

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	if _, err := rdb.Get(context.Background(), fixNowCtxKey("a")).Result(); err == nil {
		t.Error("a plain !restart must not save a fixnow context")
	}
}
