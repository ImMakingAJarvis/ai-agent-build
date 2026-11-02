package planworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// TestClientEngineOllama drives the adapter against a fake Ollama API:
// the swap sequence the handler performs (list, load, chat, unload) and
// that Loaded reports the engine's model names.
func TestClientEngineOllama(t *testing.T) {
	loaded := map[string]bool{"brain-27b": true}
	var pulls, unloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/api/ps":
			var ms []map[string]any
			for n := range loaded {
				ms = append(ms, map[string]any{"name": n})
			}
			b, _ := json.Marshal(map[string]any{"models": ms})
			w.Write(b)
		case "/api/pull":
			var req struct{ Name string `json:"name"` }
			json.NewDecoder(r.Body).Decode(&req)
			pulls = append(pulls, req.Name)
			loaded[req.Name] = true
			io.WriteString(w, `{"status":"success"}`)
		case "/api/generate":
			var req struct {
				Model     string `json:"model"`
				KeepAlive string `json:"keep_alive"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.KeepAlive == "0" { // the unload idiom
				unloads = append(unloads, req.Model)
				delete(loaded, req.Model)
			}
			io.WriteString(w, `{"response":"","done":true}`)
		case "/v1/chat/completions":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello from engine"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	eng := NewClientEngine(srv.URL)
	ctx := context.Background()

	got, err := eng.Loaded(ctx)
	if err != nil {
		t.Fatalf("Loaded: %v", err)
	}
	if len(got) != 1 || got[0] != "brain-27b" {
		t.Fatalf("Loaded = %v", got)
	}
	if err := eng.Load(ctx, "planner-a"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(pulls) != 1 || pulls[0] != "planner-a" {
		t.Fatalf("pulls = %v", pulls)
	}
	got, err = eng.Loaded(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("Loaded after load = %v (err %v)", got, err)
	}
	out, err := eng.Chat(ctx, "planner-a", []models.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "hello from engine" {
		t.Fatalf("Chat = %q", out)
	}
	if err := eng.Unload(ctx, "brain-27b"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if len(unloads) != 1 || unloads[0] != "brain-27b" {
		t.Fatalf("unloads = %v", unloads)
	}
}

// TestClientEngineLlamaCPP drives the adapter against a fake llama.cpp
// server: /api/ps 404s, /v1/models reports the GGUF path (the name the
// engine uses), unload is a no-op, and chat goes to /v1/chat/completions.
func TestClientEngineLlamaCPP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/api/ps", "/api/generate", "/api/pull":
			http.NotFound(w, r)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"/opt/models/planner-a.gguf",
				"meta":{"n_ctx":32768,"ftype":"Q4_K_M","n_params":27000000000,"size":16000000000}}]}`)
		case "/v1/completions":
			io.WriteString(w, `{"choices":[{"text":""}]}`)
		case "/v1/chat/completions":
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok from llama"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	eng := NewClientEngine(srv.URL)
	ctx := context.Background()

	got, err := eng.Loaded(ctx)
	if err != nil {
		t.Fatalf("Loaded: %v", err)
	}
	if len(got) != 1 || got[0] != "planner-a.gguf" {
		t.Fatalf("Loaded = %v, want the engine-reported name", got)
	}
	if err := eng.Load(ctx, "planner-a.gguf"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := eng.Unload(ctx, "planner-a.gguf"); err != nil {
		t.Fatalf("Unload (llama.cpp no-op): %v", err)
	}
	out, err := eng.Chat(ctx, "planner-a.gguf", []models.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "ok from llama" {
		t.Fatalf("Chat = %q", out)
	}
}
