package agentjob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contMD is a continuation.md the test writes the way a yielding round
// would — the canonical section names, real content under "In flight" and
// "Next steps" (the invariant-8 sections).
const contMD = `# Continuation — w1

## Goal
make the photos

## Done so far
- queued two photo jobs

## In flight (corr ids)
- w1-img-1: photo generation on the 4090

## What to do with each result
- w1-img-1: check the file, keep or requeue

## Next steps
- verify the photos, post the PR

## Gotchas
- none
`

// parkJob stores a job, writes its continuation, and parks it waiting on
// the two corrs the continuation names — the state a yielding round leaves.
func parkJob(t *testing.T, s *Store, id string) {
	t.Helper()
	ingest(t, s, Spec{ID: id, Title: "w", Instructions: "do the thing", Priority: 5, CreatedAt: time.Now().UTC()})
	setRunning(t, s, id)
	if err := os.WriteFile(s.ContinuationMDPath(id), []byte(contMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Yield(id, []string{"w1-img-1", "w1-img-2"}, time.Now().Add(time.Hour), "parked"); err != nil {
		t.Fatal(err)
	}
}

// wakeJob records the given results and wakes the parked job.
func wakeJob(t *testing.T, s *Store, id string, results map[string]string) {
	t.Helper()
	for corr, ref := range results {
		if err := s.RecordResult(id, corr, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Wake(id, "manual"); err != nil {
		t.Fatalf("wake: %v", err)
	}
}

// TestPromptContinuation: a woken job's next round prompt carries its
// continuation.md, the result refs, and the marker lines — before the job
// instructions (acceptance 1, EVB-16).
func TestPromptContinuation(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir()) // don't drain the real inbox
	s := newStore(t)
	parkJob(t, s, "w1")
	wakeJob(t, s, "w1", map[string]string{
		"w1-img-1": "/tmp/photos/img1.png",
		"w1-img-2": "a landscape, 1024x1024",
	})
	j, err := s.Get("w1")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Brief: func(string) string { return "RULES" }, StackStatus: func() string { return "" }, Capabilities: func() string { return "" }}
	p := r.Prompt(j, "local", "")

	for _, w := range []string{
		"=== CONTINUATION — you yielded earlier",
		"verify the photos, post the PR", // the "Next steps" section text
		"w1-img-1: photo generation on the 4090", // the "In flight" section text
		"RESULTS — what came back while the job waited:",
		"- w1-img-1: /tmp/photos/img1.png",
		"- w1-img-2: a landscape, 1024x1024",
		"jarvis-mq results flow.w1",
		"=== END CONTINUATION ===",
	} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q:\n%s", w, p)
		}
	}
	// The continuation lands between the job line and the instructions:
	// the round sees what it was doing BEFORE re-reading the job text.
	ji, ci, ti := strings.Index(p, "=== YOUR JOB THIS ROUND"), strings.Index(p, "=== CONTINUATION"), strings.Index(p, "do the thing")
	if !(ji < ci && ci < ti) {
		t.Errorf("bad ordering: job=%d continuation=%d instructions=%d", ji, ci, ti)
	}
}

// TestPromptContinuationTimeout: a deadline wake marks the results partial
// and lists the missing corr as not arrived.
func TestPromptContinuationTimeout(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	parkJob(t, s, "w2")
	if err := s.RecordResult("w2", "w1-img-1", "/tmp/photos/img1.png"); err != nil {
		t.Fatal(err)
	}
	pastDeadline(t, s, "w2")
	if _, err := s.Wake("w2", ""); err != nil {
		t.Fatalf("timeout wake: %v", err)
	}
	j, err := s.Get("w2")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Brief: func(string) string { return "" }, StackStatus: func() string { return "" }, Capabilities: func() string { return "" }}
	p := r.Prompt(j, "local", "")
	for _, w := range []string{
		"- w1-img-1: /tmp/photos/img1.png",
		"- w1-img-2: (not arrived yet)",
		"may be PARTIAL",
	} {
		if !strings.Contains(p, w) {
			t.Errorf("timeout prompt lacks %q:\n%s", w, p)
		}
	}
}

// TestPromptFreshNoContinuation: an ordinary round is prompt-identical to
// before EVB-16 — no continuation block for a job that never yielded.
func TestPromptFreshNoContinuation(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "f1", Title: "f", Instructions: "do the thing", Priority: 5, CreatedAt: time.Now().UTC()})
	j, err := s.Get("f1")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Brief: func(string) string { return "RULES" }, StackStatus: func() string { return "" }, Capabilities: func() string { return "" }}
	p := r.Prompt(j, "local", "")
	if strings.Contains(p, "CONTINUATION") {
		t.Errorf("fresh prompt must not carry a continuation block:\n%s", p)
	}
}

// TestPromptCapabilities: the round preamble carries the event toolbox
// (acceptance 2, EVB-31) — the catalog lines from the Capabilities hook
// under the CAPABILITIES header, followed by the cost-model heuristic, and
// the block sits before the job text. An empty catalog omits the block.
func TestPromptCapabilities(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "c1", Title: "c", Instructions: "do the thing", Priority: 5, CreatedAt: time.Now().UTC()})
	j, err := s.Get("c1")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Brief: func(string) string { return "" },
		StackStatus: func() string { return "" },
		Capabilities: func() string { return "image.generate\twhere=gpu\tcost=needs brain out\tdepth=0\ttypical~45s" }}
	p := r.Prompt(j, "local", "")
	for _, w := range []string{
		"CAPABILITIES (jarvis-mq caps",
		"image.generate",
		"needs brain out",
		"Cost model:",
		"submit them all before yielding",
	} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt missing %q:\n%s", w, p)
		}
	}
	capsIdx, jobIdx := strings.Index(p, "CAPABILITIES"), strings.Index(p, "YOUR JOB THIS ROUND")
	if capsIdx < 0 || jobIdx < 0 || capsIdx > jobIdx {
		t.Errorf("catalog block must precede the job text (caps=%d job=%d):\n%s", capsIdx, jobIdx, p)
	}
	r.Capabilities = func() string { return "" }
	if p := r.Prompt(j, "local", ""); strings.Contains(p, "CAPABILITIES") {
		t.Errorf("empty catalog must omit the block:\n%s", p)
	}
}

// TestRunRoundEnv: the round command's env carries the job id and the
// reply queue (acceptance 2, EVB-16) — JARVIS_JOB_ID=<id>,
// JARVIS_REPLY_QUEUE=flow.<id> — with no duplicate from the process env.
func TestRunRoundEnv(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	t.Setenv("JARVIS_REDIS", "off") // no live streaming in the test
	s := newStore(t)
	ingest(t, s, Spec{ID: "e1", Title: "e", Instructions: "x", Priority: 5, CreatedAt: time.Now().UTC()})

	// Run appends the round env to the command after Command returns, so
	// capture the *exec.Cmd and inspect it once Run is done.
	var captured *exec.Cmd
	r := &Runner{
		Store: s,
		Command: func(driver, prompt, effort string) *exec.Cmd {
			captured = exec.Command("true")
			return captured
		},
	}
	if _, err := r.Run(context.Background(), "e1", "r1", "local", ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	var jobN, qN int
	for _, kv := range captured.Env {
		switch kv {
		case "JARVIS_JOB_ID=e1":
			jobN++
		case "JARVIS_REPLY_QUEUE=flow.e1":
			qN++
		}
	}
	if jobN != 1 {
		t.Errorf("JARVIS_JOB_ID=e1 present %d times, want 1", jobN)
	}
	if qN != 1 {
		t.Errorf("JARVIS_REPLY_QUEUE=flow.e1 present %d times, want 1", qN)
	}
}

func TestStreamSessionID(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"empty", "", ""},
		{"plain output", "hello world\nfinal answer\n", ""},
		{"init only", `{"type":"system","subtype":"init","session_id":"s-init"}`, "s-init"},
		{"init and result", `{"type":"system","subtype":"init","session_id":"s-init"}
{"type":"assistant","message":{"content":[]}}
{"type":"result","subtype":"success","session_id":"s-res","result":"done"}`, "s-res"},
		{"result without sid falls back to init", `{"type":"system","subtype":"init","session_id":"s-init"}
{"type":"result","subtype":"error_max_turns"}`, "s-init"},
		{"garbage lines skipped", `not json
{"type":"system","subtype":"init","session_id":"s-init"}
{broken`, "s-init"},
	}
	for _, tc := range cases {
		if got := streamSessionID(tc.out); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRecordSessionID: a yielding round leaves its Claude Code session id
// in continuation.json (plan tasks 16/18) so a resume can re-attach it.
func TestRecordSessionID(t *testing.T) {
	s := newStore(t)
	parkJob(t, s, "w1")
	if err := s.WriteContinuation(&Continuation{JobID: "w1", WaitOn: []string{"w1-img-1"}, At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s}
	r.recordSessionID("w1", "sess-123")
	c, err := s.ReadContinuation("w1")
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionID != "sess-123" {
		t.Errorf("session id %q, want sess-123", c.SessionID)
	}
	// A second record with the same id is a no-op; a different one replaces it.
	r.recordSessionID("w1", "sess-123")
	r.recordSessionID("w1", "sess-456")
	c, _ = s.ReadContinuation("w1")
	if c.SessionID != "sess-456" {
		t.Errorf("session id %q, want sess-456", c.SessionID)
	}
	// No continuation (a round that never yielded): silently a no-op.
	ingest(t, s, Spec{ID: "w2", Title: "w", Instructions: "x", Priority: 5, CreatedAt: time.Now().UTC()})
	r.recordSessionID("w2", "sess-789")
	if _, err := s.ReadContinuation("w2"); !os.IsNotExist(err) {
		t.Errorf("ReadContinuation(w2) err %v, want ErrNotExist", err)
	}
}

// TestResumeSessionID: a woken round re-attaches its yielding session only
// when the continuation records a session id AND the session file is still
// on disk; anything less is the ordinary fresh round (EVB-18).
func TestResumeSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	s := newStore(t)
	parkJob(t, s, "w1")
	if err := s.WriteContinuation(&Continuation{JobID: "w1", WaitOn: []string{"w1-img-1", "w1-img-2"}, At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s}
	ingest(t, s, Spec{ID: "w2", Title: "w", Instructions: "x", Priority: 5, CreatedAt: time.Now().UTC()})

	// Never yielded (no continuation): fresh round.
	if got := r.resumeSessionID("w2"); got != "" {
		t.Errorf("w2: resumeSessionID %q, want empty", got)
	}
	// Yielded, but the session id was never recorded: fresh round.
	if got := r.resumeSessionID("w1"); got != "" {
		t.Errorf("w1 no-sid: got %q, want empty", got)
	}
	// Session id recorded, but the session file is gone: fresh round.
	sid := "sess-123"
	c, err := s.ReadContinuation("w1")
	if err != nil {
		t.Fatal(err)
	}
	c.SessionID = sid
	if err := s.WriteContinuation(c); err != nil {
		t.Fatal(err)
	}
	if got := r.resumeSessionID("w1"); got != "" {
		t.Errorf("w1 no-file: got %q, want empty", got)
	}
	// Session file present: resume.
	sf := claudeSessionFile(cwd, sid)
	if err := os.MkdirAll(filepath.Dir(sf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sf, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.resumeSessionID("w1"); got != sid {
		t.Errorf("w1: resumeSessionID %q, want %q", got, sid)
	}
}

// TestClaudeSessionFile: the session transcript lives under
// $HOME/.claude/projects/<slug>/<sid>.jsonl, the slug being the working
// directory with every non-alphanumeric replaced by "-".
func TestClaudeSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".claude", "projects", "-home-agent-ai-agent-build", "abc.jsonl")
	if got := claudeSessionFile("/home/agent/ai-agent-build", "abc"); got != want {
		t.Errorf("claudeSessionFile = %q, want %q", got, want)
	}
}
