package superagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func testTools(t *testing.T, workdir string) *Tools {
	t.Helper()
	tl := NewTools(workdir)
	tl.MemoryURL = "" // off by default in tests
	return tl
}

func act(tool string, args map[string]any) Action {
	b, _ := json.Marshal(args) // test args are always valid JSON
	return Action{Tool: tool, Args: b}
}

func TestShellRun(t *testing.T) {
	dir := t.TempDir()
	tl := testTools(t, dir)
	obs, err := tl.Run(context.Background(), act("shell", map[string]any{"cmd": "echo hello-superagent"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(obs, "hello-superagent") {
		t.Fatalf("obs = %q", obs)
	}
	obs, err = tl.Run(context.Background(), act("shell", map[string]any{"cmd": "exit 3"}))
	if err != nil || !strings.HasPrefix(obs, "exit 3") {
		t.Fatalf("nonzero exit: %q err=%v", obs, err)
	}
	// Runs in the work dir.
	if _, err := tl.Run(context.Background(), act("shell", map[string]any{"cmd": "touch here.txt"})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "here.txt")); err != nil {
		t.Fatalf("work dir not used: %v", err)
	}
}

func TestShellBlockedAndTimeout(t *testing.T) {
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("shell", map[string]any{"cmd": "reboot"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(obs, "BLOCKED:") {
		t.Fatalf("obs = %q", obs)
	}
	obs, err = tl.Run(context.Background(), act("shell", map[string]any{"cmd": "sleep 5", "timeout": 1}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(obs, "TIMED OUT") {
		t.Fatalf("obs = %q", obs)
	}
}

func TestWriteReadList(t *testing.T) {
	dir := t.TempDir()
	tl := testTools(t, dir)
	obs, err := tl.Run(context.Background(), act("write_file", map[string]any{
		"path": "sub/notes.txt", "content": "line one\nline two\nline three\n",
	}))
	if err != nil || !strings.Contains(obs, "wrote") {
		t.Fatalf("write: %q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("read_file", map[string]any{"path": "sub/notes.txt"}))
	if err != nil || !strings.Contains(obs, "line two") {
		t.Fatalf("read: %q err=%v", obs, err)
	}
	// offset/limit window
	obs, err = tl.Run(context.Background(), act("read_file", map[string]any{"path": "sub/notes.txt", "offset": 2, "limit": 1}))
	if err != nil || !strings.Contains(obs, "line two") || strings.Contains(obs, "line three") {
		t.Fatalf("windowed read: %q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("list_dir", map[string]any{"path": "sub"}))
	if err != nil || !strings.Contains(obs, "notes.txt") {
		t.Fatalf("list: %q err=%v", obs, err)
	}
	// Escape attempts are blocked.
	if obs, _ = tl.Run(context.Background(), act("write_file", map[string]any{"path": "../outside.txt", "content": "x"})); !strings.HasPrefix(obs, "BLOCKED:") {
		t.Fatalf("escape write: %q", obs)
	}
	if obs, _ = tl.Run(context.Background(), act("read_file", map[string]any{"path": "/etc/passwd"})); !strings.HasPrefix(obs, "BLOCKED:") {
		t.Fatalf("escape read: %q", obs)
	}
}

func TestMemorySearch(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		var q struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		gotQuery = q.Query
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "old job", "text": "we fixed the wake word latency", "score": 0.9, "path": "job-x/output.txt"},
			},
		})
	}))
	defer srv.Close()

	tl := testTools(t, t.TempDir())
	tl.MemoryURL = srv.URL
	obs, err := tl.Run(context.Background(), act("memory_search", map[string]any{"query": "wake word"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotQuery != "wake word" || !strings.Contains(obs, "old job") || !strings.Contains(obs, "0.90") {
		t.Fatalf("obs = %q (query=%q)", obs, gotQuery)
	}
	// Unreachable memory service degrades to an observation, not an error.
	tl.MemoryURL = "http://127.0.0.1:1"
	if obs, err := tl.Run(context.Background(), act("memory_search", map[string]any{"query": "x"})); err != nil || !strings.Contains(obs, "unreachable") {
		t.Fatalf("unreachable: %q err=%v", obs, err)
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("the answer is 42"))
	}))
	defer srv.Close()
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("web_fetch", map[string]any{"url": srv.URL}))
	if err != nil || !strings.Contains(obs, "42") || !strings.Contains(obs, "HTTP 200") {
		t.Fatalf("obs = %q err=%v", obs, err)
	}
	if obs, err := tl.Run(context.Background(), act("web_fetch", map[string]any{"url": "ftp://x"})); err == nil {
		t.Fatalf("want error for non-http url, got %q", obs)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("", 100); got != "(no output)" {
		t.Fatalf("empty got %q", got)
	}
	long := strings.Repeat("a", 10000) + "\nERROR: boom"
	got := truncate(long, 1000)
	if len(got) > 1100 || !strings.Contains(got, "truncated") || !strings.HasSuffix(got, "ERROR: boom") {
		t.Fatalf("got %d chars, tail kept=%v", len(got), strings.HasSuffix(got, "ERROR: boom"))
	}
}

func TestNewToolsDefaults(t *testing.T) {
	tl := NewTools("/work")
	if tl.Guard.WorkDir != "/work" || tl.MemoryURL == "" || tl.HTTP.Timeout != 60*time.Second {
		t.Fatalf("defaults: %+v", tl)
	}
}

// --- rabbitmq capability ops (EVB-32): validation is checked before any
// broker contact, so these run without RabbitMQ. ---

func TestMQBadOp(t *testing.T) {
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "explode"}))
	if err == nil || !strings.Contains(err.Error(), "must be") {
		t.Fatalf("bad op: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "depth"}))
	if err == nil || !strings.Contains(err.Error(), "needs a queue") {
		t.Fatalf("depth without queue: obs=%q err=%v", obs, err)
	}
}

func TestMQSubmitValidation(t *testing.T) {
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "submit"}))
	if err == nil || !strings.Contains(err.Error(), "needs a capability") {
		t.Fatalf("missing capability: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "submit", "capability": "Image_Describe"}))
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("bad capability: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "submit", "capability": "echo", "deadline": "soon"}))
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("bad deadline: obs=%q err=%v", obs, err)
	}
}

func TestMQListenValidation(t *testing.T) {
	tl := testTools(t, t.TempDir())
	t.Setenv("JARVIS_JOB_ID", "")
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "listen"}))
	if err == nil || !strings.Contains(err.Error(), "needs a queue") {
		t.Fatalf("no queue: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "listen", "queue": "flow.x"}))
	if err == nil || !strings.Contains(err.Error(), "needs a job") {
		t.Fatalf("wake without job: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "listen", "queue": "flow.x", "action": "forward"}))
	if err == nil || !strings.Contains(err.Error(), "needs a target queue") {
		t.Fatalf("forward without target: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "listen", "queue": "flow.x", "action": "explode"}))
	if err == nil || !strings.Contains(err.Error(), "must be wake, forward or notify") {
		t.Fatalf("bad action: obs=%q err=%v", obs, err)
	}
}

func TestMQYieldValidation(t *testing.T) {
	tl := testTools(t, t.TempDir())
	t.Setenv("JARVIS_DATA_DIR", t.TempDir()) // an isolated job store
	t.Setenv("JARVIS_JOB_ID", "")
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "yield", "wait-on": "c1", "note": "n"}))
	if err == nil || !strings.Contains(err.Error(), "needs an agent job") {
		t.Fatalf("yield without job: obs=%q err=%v", obs, err)
	}
	t.Setenv("JARVIS_JOB_ID", "evb32-no-such-job")
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "yield"}))
	if err == nil || !strings.Contains(err.Error(), "needs wait-on") {
		t.Fatalf("yield without wait-on: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "yield", "wait-on": "c1"}))
	if err == nil || !strings.Contains(err.Error(), "needs a note") {
		t.Fatalf("yield without note: obs=%q err=%v", obs, err)
	}
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "yield", "wait-on": "c1", "note": "n"}))
	if err == nil || !strings.Contains(err.Error(), "yield") {
		t.Fatalf("yield unknown job: obs=%q err=%v", obs, err)
	}
}

func TestMQCaps(t *testing.T) {
	tl := testTools(t, t.TempDir())
	// A missing catalog file degrades to an observation, not an error.
	t.Setenv("JARVIS_CAPABILITIES", filepath.Join(t.TempDir(), "absent.yaml"))
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "caps"}))
	if err != nil || !strings.Contains(obs, "caps:") {
		t.Fatalf("missing catalog: obs=%q err=%v", obs, err)
	}
	// The repo catalog renders one line per capability (depths are "-"
	// when the broker is down).
	cat := filepath.Join("..", "..", "config", "capabilities.yaml")
	if _, err := os.Stat(cat); err != nil {
		t.Skipf("no repo catalog: %v", err)
	}
	t.Setenv("JARVIS_CAPABILITIES", cat)
	obs, err = tl.Run(context.Background(), act("rabbitmq", map[string]any{"op": "caps"}))
	if err != nil || !strings.Contains(obs, "image.describe") || !strings.Contains(obs, "where=") {
		t.Fatalf("caps: obs=%q err=%v", obs, err)
	}
}

func TestMQSubmitLive(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	c.Close()
	tl := testTools(t, t.TempDir())
	corr := "evb32-" + saRandSuffix()
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{
		"op": "submit", "capability": "echo", "corr": corr,
		"input": map[string]any{"text": "evb32"},
	}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.Contains(obs, "submitted echo (corr "+corr+") → cap.echo") {
		t.Fatalf("obs = %q", obs)
	}
}

func TestMQListenLive(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	c.Close()
	listenDir := t.TempDir()
	t.Setenv("LISTENERS_DIR", listenDir) // the real switchboard store stays untouched
	jobID := "evb32-listen-" + saRandSuffix()
	q := "flow." + jobID
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{
		"op": "listen", "queue": q, "action": "wake", "corr": "c1", "job": jobID,
	}))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !strings.Contains(obs, "listening on "+q) || !strings.Contains(obs, "c1") {
		t.Fatalf("obs = %q", obs)
	}
	// The listener must be persisted — the switchboard reads it from disk.
	if entries, err := os.ReadDir(listenDir); err != nil || len(entries) == 0 {
		t.Fatalf("no persisted listener in %s: %v", listenDir, err)
	}
}

func TestMQYieldLive(t *testing.T) {
	data := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", data) // an isolated job store
	st, err := agentjob.Open(data)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	jobID := "evb32-yield-" + saRandSuffix()
	if _, err := st.Ingest(agentjob.Spec{ID: jobID, Title: "evb32 yield test"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := st.Claim(jobID, "r1", "test"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := st.SetStatus(jobID, agentjob.State{Status: agentjob.Running, RanWith: "test", Round: "r1"}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	t.Setenv("JARVIS_JOB_ID", jobID)
	tl := testTools(t, t.TempDir())
	obs, err := tl.Run(context.Background(), act("rabbitmq", map[string]any{
		"op": "yield", "wait-on": "c1,c2", "note": "consume the results and finish", "deadline": "30m",
	}))
	if err != nil {
		t.Fatalf("yield: %v", err)
	}
	if !tl.Yielded {
		t.Fatal("Tools.Yielded not set after a yield")
	}
	if !strings.Contains(obs, "yielded: job "+jobID) || !strings.Contains(obs, "c1,c2") {
		t.Fatalf("obs = %q", obs)
	}
	j, err := st.Get(jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != agentjob.Waiting || len(j.WaitOn) != 2 {
		t.Fatalf("job = %q waitOn=%v, want waiting on 2", j.Status, j.WaitOn)
	}
}
