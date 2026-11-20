package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
)

func TestCutIGPU(t *testing.T) {
	cases := []struct {
		in   string
		task string
		ok   bool
	}{
		{"!igpu restart the brain", "restart the brain", true},
		{"!igpu   docker start jarvis-engine  ", "docker start jarvis-engine", true},
		{"!igpu\tjarvis-swap status", "jarvis-swap status", true},
		{"!igpu\njarvis-swap in\nthen report", "jarvis-swap in\nthen report", true},
		{"!iGPU run jarvis-swap status", "run jarvis-swap status", true}, // case-insensitive
		{"!IGPU x", "x", true},
		{"!igpu", "", true}, // the command, but no task
		{"!igpu   ", "", true},
		{"!igpus are slow", "", false}, // word boundary required
		{"!igpunow x", "", false},
		{"!igpu-bench", "", false},
		{"hello !igpu x", "", false}, // must start the message
		{"igpu x", "", false},
		{"!ig", "", false},
		{"", "", false},
		// the other commands are different: !igpu must not claim them.
		{"!fix the fan curve", "", false},
		{"!build x", "", false},
		{"!photo a red panda", "", false},
	}
	for _, c := range cases {
		task, ok := cutIGPU(c.in)
		if ok != c.ok || task != c.task {
			t.Errorf("cutIGPU(%q) = %q %v; want %q %v", c.in, task, ok, c.task, c.ok)
		}
	}
	// And the reverse: the queueing and photo cutters must not claim !igpu.
	if _, _, ok := cutBuildFix("!igpu restart the brain"); ok {
		t.Error("cutBuildFix claimed !igpu")
	}
	if _, _, _, ok := cutPhotoCmd("!igpu restart the brain"); ok {
		t.Error("cutPhotoCmd claimed !igpu")
	}
}

func TestIGPUTitle(t *testing.T) {
	if got := igpuTitle("  restart the brain \nthen tell me"); got != "igpu: restart the brain" {
		t.Errorf("title = %q", got)
	}
	// A long first line is cut to 80 bytes without splitting a rune.
	long := strings.Repeat("é", 100) // 2 bytes each
	got := igpuTitle(long)
	body := strings.TrimPrefix(got, "igpu: ")
	if len(body) > 80 || !utf8.ValidString(body) || body == "" {
		t.Errorf("long title body = %d bytes, valid=%v", len(body), utf8.ValidString(body))
	}
}

func TestEnqueueIGPUJob(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	in := chat.Incoming{ChannelID: "c1", MessageID: "m1"}
	task := "run `jarvis-swap in`\nthen `jarvis-swap status` and tell me the last line"
	id, err := enqueueIGPUJob(in, task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "chat-") {
		t.Fatalf("id %q", id)
	}
	s, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	// Routed explicitly to the iGPU worker: the spec driver is what
	// `jarvis-jobs next` gates on (the iGPU engine, never the brain or the
	// game-hold marker) and what agent-run runs the round on.
	if j.Driver != "igpu" {
		t.Errorf("driver = %q, want igpu", j.Driver)
	}
	if !j.HasTag(agentjob.TagIGPUDirect) {
		t.Errorf("tags = %v, want %q", j.Tags, agentjob.TagIGPUDirect)
	}
	if j.HasTag(agentjob.TagPhoto) || j.HasTag(agentjob.TagNightly) {
		t.Errorf("tags = %v: an !igpu job is neither photo nor nightly work", j.Tags)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("status = %q, want ready", j.Status)
	}
	if j.Class != agentjob.ClassFix || j.Priority != igpuJobPriority {
		t.Errorf("class/priority = %q/%d, want fix/%d", j.Class, j.Priority, igpuJobPriority)
	}
	if j.Priority < fixNowJobPriority {
		t.Errorf("igpu priority %d must not be below fixnow %d", j.Priority, fixNowJobPriority)
	}
	if j.Source != "discord" || j.DiscordChannelID != "c1" || j.DiscordMessageID != "m1" {
		t.Errorf("source/discord ids = %q %q %q", j.Source, j.DiscordChannelID, j.DiscordMessageID)
	}
	if j.Title != "igpu: run `jarvis-swap in`" {
		t.Errorf("title = %q", j.Title)
	}
	// The whole task — every line, not just the text after the title line —
	// reaches the round, with the report-back instruction naming this job.
	for _, want := range []string{
		"run `jarvis-swap in`",
		"then `jarvis-swap status` and tell me the last line",
		`jarvis-discord notify "igpu job ` + id + ` done:`,
		`jarvis-discord notify "igpu job ` + id + ` FAILED:`,
		"no PR",
	} {
		if !strings.Contains(j.Instructions, want) {
			t.Errorf("instructions missing %q:\n%s", want, j.Instructions)
		}
	}
	// Queueing must never touch the running round.
	for _, sig := range []string{"pause-signal", "restart-signal"} {
		if _, err := os.Stat(filepath.Join(dir, sig)); !os.IsNotExist(err) {
			t.Errorf("%s must not be created", sig)
		}
	}
}

func TestEnqueueIGPUJobRejectsEmpty(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	if _, err := enqueueIGPUJob(chat.Incoming{}, "  \n "); err == nil {
		t.Fatal("empty task must be rejected")
	}
}

// An !igpu job is the next job the loop pulls: ahead of ordinary fix and
// build work that was queued before it.
func TestIGPUJobRunsBeforeQueuedWork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	in := chat.Incoming{ChannelID: "c1", MessageID: "m1"}
	if _, err := enqueueFromChat(in, agentjob.ClassFix, "an ordinary fix", "body", chatJobPriority); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueueFromChat(in, agentjob.ClassBuild, "a build", "body", chatJobPriority); err != nil {
		t.Fatal(err)
	}
	id, err := enqueueIGPUJob(in, "docker start jarvis-engine")
	if err != nil {
		t.Fatal(err)
	}
	s, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.ID != id {
		t.Fatalf("next job = %v, want the !igpu job %s", next, id)
	}
}

// healthServer answers /health with the given status.
func healthServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probe path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func igpuJobs(t *testing.T, dir string) []agentjob.Job {
	t.Helper()
	s, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestHandleIGPUQueuesAndConfirms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	t.Setenv("IGPU_URL", healthServer(t, http.StatusOK).URL)
	t.Setenv("FALLBACK_ENGINE_URL", "http://127.0.0.1:1") // IGPU_URL wins

	d := &testDiscord{}
	in := chat.Incoming{ChannelID: "c1", MessageID: "m1", Content: "  !igpu docker start jarvis-engine\n"}
	if !handleIGPU(d, in) {
		t.Fatal("handleIGPU did not claim the !igpu message")
	}
	jobs := igpuJobs(t, dir)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Driver != "igpu" || !strings.Contains(jobs[0].Instructions, "docker start jarvis-engine") {
		t.Errorf("job = %+v", jobs[0].Spec)
	}
	got := d.lastNotify()
	for _, want := range []string{jobs[0].ID, "igpu driver", "game hold"} {
		if !strings.Contains(got, want) {
			t.Errorf("confirmation missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "Warning") {
		t.Errorf("healthy iGPU engine must not warn: %q", got)
	}
}

// The recovery command works while a checkpoint question is open: the
// ask-open marker gates ordinary chat, not !igpu (handleIGPU runs ahead of
// that gate in the OnMessage hook and never reads the marker).
func TestHandleIGPUIgnoresAskOpen(t *testing.T) {
	dir := t.TempDir()
	conf := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	t.Setenv("JARVIS_CONF", conf)
	t.Setenv("IGPU_URL", "")
	t.Setenv("FALLBACK_ENGINE_URL", "")
	if err := os.WriteFile(filepath.Join(conf, "ask-open"), []byte("checkpoint question open"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &testDiscord{}
	if !handleIGPU(d, chat.Incoming{Content: "!igpu jarvis-swap status"}) {
		t.Fatal("handleIGPU did not claim the !igpu message")
	}
	if n := len(igpuJobs(t, dir)); n != 1 {
		t.Fatalf("jobs = %d, want 1", n)
	}
	// No engine URL configured: the reply says nothing about the engine.
	if got := d.lastNotify(); strings.Contains(got, "Warning") {
		t.Errorf("unknown engine state must not warn: %q", got)
	}
}

func TestHandleIGPUWarnsWhenEngineDown(t *testing.T) {
	for name, url := range map[string]string{
		"unreachable": "http://127.0.0.1:1",
		"loading":     healthServer(t, http.StatusServiceUnavailable).URL,
	} {
		dir := t.TempDir()
		t.Setenv("JARVIS_DATA_DIR", dir)
		t.Setenv("IGPU_URL", "")
		t.Setenv("FALLBACK_ENGINE_URL", url+"/") // the compose default source, trailing slash tolerated
		d := &testDiscord{}
		if !handleIGPU(d, chat.Incoming{Content: "!igpu jarvis-swap status"}) {
			t.Fatalf("%s: handleIGPU did not claim the message", name)
		}
		// The job is still queued — it runs once the engine answers.
		if n := len(igpuJobs(t, dir)); n != 1 {
			t.Fatalf("%s: jobs = %d, want 1", name, n)
		}
		if got := d.lastNotify(); !strings.Contains(got, "Warning: the iGPU engine is not answering") {
			t.Errorf("%s: confirmation has no engine warning: %q", name, got)
		}
	}
}

func TestHandleIGPUEmptyTask(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	d := &testDiscord{}
	if !handleIGPU(d, chat.Incoming{Content: "!igpu"}) {
		t.Fatal("a bare !igpu is still the command")
	}
	if got := d.lastNotify(); !strings.Contains(got, "Need a task after !igpu") {
		t.Errorf("usage reply = %q", got)
	}
	if n := len(igpuJobs(t, dir)); n != 0 {
		t.Fatalf("jobs = %d, want 0", n)
	}
}

func TestHandleIGPUNotTheCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	d := &testDiscord{}
	for _, content := range []string{"hello", "!igpus are slow", "!fix the fan curve", "what does !igpu do?", ""} {
		if handleIGPU(d, chat.Incoming{Content: content}) {
			t.Errorf("handleIGPU claimed %q", content)
		}
	}
	if len(d.notifyMsgs) != 0 {
		t.Errorf("replies = %v, want none", d.notifyMsgs)
	}
	if n := len(igpuJobs(t, dir)); n != 0 {
		t.Fatalf("jobs = %d, want 0", n)
	}
}

func TestHandleIGPUEnqueueError(t *testing.T) {
	// A data dir that cannot hold the store: the owner is told, nothing is
	// queued, and the message is still consumed (not sent to the brain).
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_DATA_DIR", blocker)
	d := &testDiscord{}
	if !handleIGPU(d, chat.Incoming{Content: "!igpu jarvis-swap status"}) {
		t.Fatal("handleIGPU did not claim the message")
	}
	if got := d.lastNotify(); !strings.Contains(got, "Could not enqueue the iGPU job") {
		t.Errorf("error reply = %q", got)
	}
}

func TestIGPUConfirm(t *testing.T) {
	up := igpuConfirm("chat-1-abc", true, true, true)
	for _, want := range []string{"chat-1-abc", "top of the fix band", "agent-run is up", "reports the result here"} {
		if !strings.Contains(up, want) {
			t.Errorf("igpuConfirm missing %q: %q", want, up)
		}
	}
	down := igpuConfirm("chat-1-abc", false, false, true)
	for _, want := range []string{"chat-1-abc", "agent-run is not running", "!restart-service", "Warning: the iGPU engine"} {
		if !strings.Contains(down, want) {
			t.Errorf("igpuConfirm (loop and engine down) missing %q: %q", want, down)
		}
	}
	if unknown := igpuConfirm("chat-1-abc", true, false, false); strings.Contains(unknown, "Warning") {
		t.Errorf("unknown engine state must not warn: %q", unknown)
	}
}
