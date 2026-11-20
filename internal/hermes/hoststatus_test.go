package hermes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// llamaServer mimics a llama.cpp engine: /health → {"status":"ok"},
// /v1/models → the loaded model (reported by llama.cpp as a GGUF path).
func llamaServer(t *testing.T, modelPath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v1/models":
			models := []map[string]string{}
			if modelPath != "" {
				models = append(models, map[string]string{"name": modelPath})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		default:
			http.NotFound(w, r)
		}
	}))
}

// ollamaServer mimics an Ollama-shaped engine: no /health, but /api/tags
// with the model list (the fallback probe in engineUp).
func ollamaServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			var models []map[string]string
			if name != "" {
				models = append(models, map[string]string{"name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestEngineHealth_UpLlamaCpp(t *testing.T) {
	srv := llamaServer(t, "/opt/jarvis-engine/models/jarvis-brain.gguf")
	defer srv.Close()
	t.Setenv("ENGINE_URL", srv.URL)
	t.Setenv("FALLBACK_ENGINE_URL", "")
	if got := engineHealth(context.Background()); got != "ok (jarvis-brain)" {
		t.Errorf("engineHealth = %q, want %q", got, "ok (jarvis-brain)")
	}
}

func TestEngineHealth_UpNoModel(t *testing.T) {
	srv := llamaServer(t, "")
	defer srv.Close()
	t.Setenv("ENGINE_URL", srv.URL)
	t.Setenv("FALLBACK_ENGINE_URL", "")
	if got := engineHealth(context.Background()); got != "ok" {
		t.Errorf("engineHealth = %q, want %q", got, "ok")
	}
}

func TestEngineHealth_UpOllamaShape(t *testing.T) {
	srv := ollamaServer(t, "qwen2.5:3b")
	defer srv.Close()
	t.Setenv("ENGINE_URL", srv.URL)
	t.Setenv("FALLBACK_ENGINE_URL", "")
	// The engine is up via the /api/tags probe; the model name comes from
	// /v1/models, which this stub does not serve — "ok" without a name.
	if got := engineHealth(context.Background()); got != "ok" {
		t.Errorf("engineHealth = %q, want %q", got, "ok")
	}
}

func TestEngineHealth_UnreachableWithFallback(t *testing.T) {
	fb := llamaServer(t, "/opt/jarvis-igpu/models/jarvis-igpu.gguf")
	defer fb.Close()
	t.Setenv("ENGINE_URL", "http://127.0.0.1:1") // nothing listens on port 1
	t.Setenv("FALLBACK_ENGINE_URL", fb.URL)
	got := engineHealth(context.Background())
	want := "unreachable (chat running on iGPU fallback)"
	if got != want {
		t.Errorf("engineHealth = %q, want %q", got, want)
	}
}

func TestEngineHealth_DownNoFallback(t *testing.T) {
	t.Setenv("ENGINE_URL", "http://127.0.0.1:1")
	t.Setenv("FALLBACK_ENGINE_URL", "")
	if got := engineHealth(context.Background()); got != "unreachable" {
		t.Errorf("engineHealth = %q, want %q", got, "unreachable")
	}
}

func TestEngineHealth_WedgedEngineDoesNotHang(t *testing.T) {
	// A server that accepts the connection but never answers must not hang
	// the Discord reply — each probe has a short client timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer srv.Close()
	t.Setenv("ENGINE_URL", srv.URL)
	t.Setenv("FALLBACK_ENGINE_URL", "")
	start := time.Now()
	if got := engineHealth(context.Background()); got != "unreachable" {
		t.Errorf("engineHealth = %q, want %q", got, "unreachable")
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Errorf("engineHealth took %v, want < 7s", d)
	}
}

func TestQueueSnapshot_Empty(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	summary, current, err := queueSnapshot()
	if err != nil {
		t.Fatalf("queueSnapshot: %v", err)
	}
	if summary != "(empty)" || current != "" {
		t.Errorf("queueSnapshot = %q, %q; want (empty), %q", summary, current, "")
	}
}

func TestQueueSnapshot_Populated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	store, err := agentjob.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now().UTC()
	mustIngest := func(sp agentjob.Spec) {
		t.Helper()
		if _, err := store.Ingest(sp); err != nil {
			t.Fatalf("Ingest %s: %v", sp.ID, err)
		}
	}
	mustIngest(agentjob.Spec{ID: "running-1", Title: "the big one", Class: agentjob.ClassFix, Priority: 1, CreatedAt: now})
	mustIngest(agentjob.Spec{ID: "ready-1", Title: "next up", Class: agentjob.ClassBuild, Priority: 1, CreatedAt: now})
	mustIngest(agentjob.Spec{ID: "done-1", Title: "finished long ago", Class: agentjob.ClassBuild, Priority: 1, CreatedAt: now})
	if _, err := store.Claim("running-1", "r1", "local"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Claim leaves the job claimed; the run loop moves it to running
	// (internal/agentjob/run.go).
	if err := store.SetStatus("running-1", agentjob.State{Status: agentjob.Running, PID: os.Getpid()}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := store.SetStatus("done-1", agentjob.State{Status: agentjob.Done}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	summary, current, err := queueSnapshot()
	if err != nil {
		t.Fatalf("queueSnapshot: %v", err)
	}
	want := "1 running, 1 ready"
	if summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
	if current != "the big one" {
		t.Errorf("current = %q, want %q", current, "the big one")
	}
}

// TestAgentRunLoopUp_NoPanic guards the pgrep call on machines where procps
// may be missing (it must report "down", not crash the status reply).
func TestAgentRunLoopUp_NoPanic(t *testing.T) {
	_ = AgentRunLoopUp()
}
