package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// fakeEngine serves the llama.cpp /health and OpenAI /v1/chat/completions
// endpoints and records which model each completion requested.
type fakeEngine struct {
	server   *httptest.Server
	reqModel chan string
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	reqModel := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			w.Header().Set("Content-Type", "application/json")
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&req)
			}
			reqModel <- req.Model
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"hi from %s"}}]}`, req.Model)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &fakeEngine{server: srv, reqModel: reqModel}
}

func writeJobState(t *testing.T, dir, id, status string) {
	t.Helper()
	jobDir := filepath.Join(dir, id)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "state.json"),
		[]byte(fmt.Sprintf(`{"status": %q}`, status)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestJobsDir(t *testing.T, states ...string) string {
	t.Helper()
	dir := t.TempDir()
	for i, s := range states {
		writeJobState(t, dir, fmt.Sprintf("job-%d", i), s)
	}
	return dir
}

func newTestFallback(primaryURL string, fbURL string, fbModel string, jobsDir string) *fallbackClient {
	return &fallbackClient{
		primary: models.NewClientWithBase(primaryURL),
		fb:      models.NewClientWithBase(fbURL),
		fbURL:   fbURL,
		fbModel: fbModel,
		jobsDir: jobsDir,
	}
}

func TestWorkerBusy(t *testing.T) {
	t.Run("running job", func(t *testing.T) {
		fc := newTestFallback("http://primary", "http://fb", "jarvis-igpu",
			newTestJobsDir(t, "completed", "running", "queued"))
		busy, err := fc.workerBusy()
		if err != nil || !busy {
			t.Fatalf("workerBusy = %v, %v; want true", busy, err)
		}
	})

	t.Run("no running job", func(t *testing.T) {
		fc := newTestFallback("http://primary", "http://fb", "jarvis-igpu",
			newTestJobsDir(t, "completed", "queued"))
		busy, err := fc.workerBusy()
		if err != nil || busy {
			t.Fatalf("workerBusy = %v, %v; want false", busy, err)
		}
	})

	t.Run("malformed state skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeJobState(t, dir, "ok", "completed")
		jobDir := filepath.Join(dir, "broken")
		if err := os.MkdirAll(jobDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jobDir, "state.json"), []byte("{nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		fc := newTestFallback("http://primary", "http://fb", "jarvis-igpu", dir)
		busy, err := fc.workerBusy()
		if err != nil || busy {
			t.Fatalf("workerBusy = %v, %v; want false", busy, err)
		}
	})
}

func TestChatRoutesToFallbackWhenWorkerBusy(t *testing.T) {
	primary := newFakeEngine(t)
	fb := newFakeEngine(t)
	fc := newTestFallback(primary.server.URL, fb.server.URL, "jarvis-igpu",
		newTestJobsDir(t, "running"))

	reply, err := fc.Chat("jarvis-brain", []models.ChatMessage{
		{Role: "user", Content: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-fb.reqModel; got != "jarvis-igpu" {
		t.Fatalf("fallback got model %q; want jarvis-igpu", got)
	}
	if reply != "hi from jarvis-igpu" {
		t.Fatalf("reply = %q", reply)
	}
	select {
	case got := <-primary.reqModel:
		t.Fatalf("primary engine was hit (model %q); want no call", got)
	default:
	}
}

func TestChatRoutesToPrimaryWhenWorkerIdle(t *testing.T) {
	primary := newFakeEngine(t)
	fb := newFakeEngine(t)
	fc := newTestFallback(primary.server.URL, fb.server.URL, "jarvis-igpu",
		newTestJobsDir(t, "completed"))

	if _, err := fc.Chat("jarvis-brain", []models.ChatMessage{
		{Role: "user", Content: "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-primary.reqModel; got != "jarvis-brain" {
		t.Fatalf("primary got model %q; want jarvis-brain", got)
	}
	select {
	case got := <-fb.reqModel:
		t.Fatalf("fallback was hit (model %q); want no call", got)
	default:
	}
}

func TestChatRoutesToPrimaryWhenFallbackDown(t *testing.T) {
	primary := newFakeEngine(t)
	fb := newFakeEngine(t)
	fb.server.Close() // /health unreachable
	fc := newTestFallback(primary.server.URL, fb.server.URL, "jarvis-igpu",
		newTestJobsDir(t, "running"))

	reply, err := fc.Chat("jarvis-brain", []models.ChatMessage{
		{Role: "user", Content: "hello"},
	})
	if err != nil {
		t.Fatalf("fallback down must not break chat: %v", err)
	}
	if reply != "hi from jarvis-brain" {
		t.Fatalf("reply = %q", reply)
	}
	if got := <-primary.reqModel; got != "jarvis-brain" {
		t.Fatalf("primary got model %q; want jarvis-brain", got)
	}
}

func TestChatPassthroughWithoutFallbackConfig(t *testing.T) {
	primary := newFakeEngine(t)
	fc := &fallbackClient{primary: models.NewClientWithBase(primary.server.URL)}

	if _, err := fc.Chat("jarvis-brain", []models.ChatMessage{
		{Role: "user", Content: "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-primary.reqModel; got != "jarvis-brain" {
		t.Fatalf("primary got model %q; want jarvis-brain", got)
	}
}
