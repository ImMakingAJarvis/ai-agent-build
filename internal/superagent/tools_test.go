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
