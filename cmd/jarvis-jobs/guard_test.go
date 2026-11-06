package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// newGuardJobsStore opens a throwaway job store with one failed job
// ("auto job that just died") and returns it.
func newGuardJobsStore(t *testing.T) *agentjob.Store {
	t.Helper()
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(agentjob.Spec{ID: "job-x", Title: "auto job that just died", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("job-x", agentjob.State{Status: agentjob.Failed, Error: "agent exited with status 1"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// testFlags dials the NoBrakes flag store at the test's JARVIS_REDIS
// (set by newFullAutoRedis or t.Setenv in each test).
func testFlags(t *testing.T) *nobrakes.Store {
	t.Helper()
	s, err := nobrakes.New(agentjob.StreamAddr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCmdAutoPausePausesAndNotifies(t *testing.T) {
	newFullAutoRedis(t)
	// Both switches are on before the failure.
	flags := testFlags(t)
	ctx := context.Background()
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := flags.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}
	store := newGuardJobsStore(t)
	var got string
	if err := cmdAutoPause(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatal(err)
	}
	v, err := flags.GetAutoMode(ctx)
	if err != nil || v != nobrakes.AutoModeOff {
		t.Fatalf("autopause left auto_mode = %q (%v)", v, err)
	}
	v, err = flags.GetFullAuto(ctx)
	if err != nil || v != nobrakes.AutoModeOff {
		t.Fatalf("autopause left full_auto = %q (%v)", v, err)
	}
	for _, want := range []string{"auto job that just died", "job-x", "agent exited with status 1", "paused"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notification %q missing %q", got, want)
		}
	}
}

func TestCmdAutoPauseRedisDisabledStillNotifies(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	store := newGuardJobsStore(t)
	var got string
	if err := cmdAutoPause(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatalf("disabled redis must not fail the notification: %v", err)
	}
	if !strings.Contains(got, "job-x") {
		t.Fatalf("notification not sent: %q", got)
	}
}

func TestCmdAutoPauseUnreachableRedisStillNotifies(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "redis://127.0.0.1:1")
	store := newGuardJobsStore(t)
	var got string
	if err := cmdAutoPause(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatalf("unreachable redis must not fail the notification: %v", err)
	}
	if !strings.Contains(got, "agent exited with status 1") {
		t.Fatalf("notification not sent: %q", got)
	}
}

func TestCmdAutoPauseNotifyFailureIsError(t *testing.T) {
	newFullAutoRedis(t)
	store := newGuardJobsStore(t)
	err := cmdAutoPause(store, []string{"job-x"}, func(string) error { return errors.New("discord down") })
	if err == nil {
		t.Fatal("notify failure must be an error")
	}
	if !strings.Contains(err.Error(), "discord down") {
		t.Fatalf("error = %v, want the notify failure", err)
	}
}

func TestCmdAutoPauseMissingJobIsError(t *testing.T) {
	newFullAutoRedis(t)
	store := newGuardJobsStore(t)
	if err := cmdAutoPause(store, []string{"nope"}, func(string) error { return nil }); err == nil {
		t.Fatal("unknown job id must be an error")
	}
}

// TestCmdFailNoteNotifiesWithoutPausing pins the keep-going failure path:
// the owner hears about the failure, but no flag is written — auto_mode
// and full_auto are left exactly as they were.
func TestCmdFailNoteNotifiesWithoutPausing(t *testing.T) {
	newFullAutoRedis(t)
	flags := testFlags(t)
	ctx := context.Background()
	if err := flags.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := flags.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}
	store := newGuardJobsStore(t)
	var got string
	if err := cmdFailNote(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatal(err)
	}
	// The failure is named, and the keep-going tail explains what happens
	// next.
	for _, want := range []string{"auto job that just died", "job-x", "agent exited with status 1", "Keep going is on"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notification %q missing %q", got, want)
		}
	}
	// No flag writes: both execution switches are still on.
	if v, _ := flags.GetAutoMode(ctx); v != nobrakes.AutoModeOn {
		t.Fatalf("failnote touched auto_mode = %q", v)
	}
	if v, _ := flags.GetFullAuto(ctx); v != nobrakes.AutoModeOn {
		t.Fatalf("failnote touched full_auto = %q", v)
	}
}

func TestCmdFailNoteRedisDisabledStillNotifies(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	store := newGuardJobsStore(t)
	var got string
	if err := cmdFailNote(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatalf("disabled redis must not fail the notification: %v", err)
	}
	if !strings.Contains(got, "agent exited with status 1") {
		t.Fatalf("notification not sent: %q", got)
	}
}

func TestCmdFailNoteUnreachableRedisStillNotifies(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "redis://127.0.0.1:1")
	store := newGuardJobsStore(t)
	var got string
	if err := cmdFailNote(store, []string{"job-x"}, func(m string) error { got = m; return nil }); err != nil {
		t.Fatalf("unreachable redis must not fail the notification: %v", err)
	}
	if !strings.Contains(got, "job-x") {
		t.Fatalf("notification not sent: %q", got)
	}
}

func TestCmdFailNoteNotifyFailureIsError(t *testing.T) {
	newFullAutoRedis(t)
	store := newGuardJobsStore(t)
	err := cmdFailNote(store, []string{"job-x"}, func(string) error { return errors.New("discord down") })
	if err == nil {
		t.Fatal("notify failure must be an error")
	}
	if !strings.Contains(err.Error(), "discord down") {
		t.Fatalf("error = %v, want the notify failure", err)
	}
}

func TestCmdFailNoteMissingJobIsError(t *testing.T) {
	newFullAutoRedis(t)
	store := newGuardJobsStore(t)
	if err := cmdFailNote(store, []string{"nope"}, func(string) error { return nil }); err == nil {
		t.Fatal("unknown job id must be an error")
	}
}

func TestCmdRunningNoneWhenFree(t *testing.T) {
	newFullAutoRedis(t)
	var out strings.Builder
	if err := cmdRunning(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "(none)\n" {
		t.Fatalf("running = %q, want (none)", got)
	}
}

func TestCmdRunningShowsHolderAndClears(t *testing.T) {
	mr := newFullAutoRedis(t)
	mr.Set("nobrakes:running", "job-a")
	var out strings.Builder
	if err := cmdRunning(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "job-a\n" {
		t.Fatalf("running = %q, want job-a", got)
	}
	out.Reset()
	if err := cmdRunning(&out, []string{"clear"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "cleared\n" {
		t.Fatalf("running clear = %q", got)
	}
	out.Reset()
	if err := cmdRunning(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "(none)\n" {
		t.Fatalf("running after clear = %q, want (none)", got)
	}
}

func TestCmdRunningRedisDisabled(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	var out strings.Builder
	if err := cmdRunning(&out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(none)") {
		t.Fatalf("running with JARVIS_REDIS=off = %q", out.String())
	}
}

func TestRunAutoRefusesWithoutRedis(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	store := newGuardJobsStore(t)
	err := runAuto(store, "job-x", "local", "1", "")
	if err == nil {
		t.Fatal("run -auto with no redis must not run unguarded")
	}
	if errors.Is(err, errAutoHeld) {
		t.Fatal("no-redis refusal misreported as a held marker")
	}
}

func TestRunAutoHeldByLiveJob(t *testing.T) {
	mr := newFullAutoRedis(t)
	store := newGuardJobsStore(t)
	// job-x is running and holds the marker.
	if err := store.SetStatus("job-x", agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatal(err)
	}
	mr.Set("nobrakes:running", "job-x")
	err := runAuto(store, "job-y", "local", "1", "")
	if !errors.Is(err, errAutoHeld) {
		t.Fatalf("want errAutoHeld, got %v", err)
	}
	if !strings.Contains(err.Error(), "job-x") {
		t.Fatalf("held error must name the holder: %v", err)
	}
}

func TestRunAutoStaleHolderProbesDead(t *testing.T) {
	// Same marker, but the holder's job is no longer running: the marker
	// is stale and would be taken over (the takeover itself is covered in
	// internal/nobrakes). Here we check the liveness probe the cmd layer
	// feeds AcquireRunning: a non-running holder must probe as dead.
	mr := newFullAutoRedis(t)
	store := newGuardJobsStore(t) // job-x is "failed" — dead
	mr.Set("nobrakes:running", "job-x")
	alive := func(holder string) bool {
		j, err := store.Get(holder)
		return err == nil && j.Status == agentjob.Running
	}
	if alive("job-x") {
		t.Fatal("a failed holder must probe as dead")
	}
	if alive("ghost") {
		t.Fatal("an unknown holder must probe as dead")
	}
}
