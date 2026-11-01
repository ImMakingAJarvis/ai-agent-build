package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewClientEngineURLPrecedence is the acceptance test for the
// containerized-stack bug: inside a container the engine is at
// http://engine:11434 (ENGINE_URL), never at localhost. NewClient must
// prefer ENGINE_URL over OLLAMA_URL, as cmd/jarvis-reply and
// internal/slots already do.
func TestNewClientEngineURLPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		engineURL  string
		ollamaURL  string
		wantBase   string
	}{
		{"engine wins over ollama", "http://engine:11434", "http://other:9", "http://engine:11434"},
		{"ollama when no engine", "", "http://other:9", "http://other:9"},
		{"default when neither", "", "", defaultOllamaURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENGINE_URL", tc.engineURL)
			t.Setenv("OLLAMA_URL", tc.ollamaURL)
			if c := NewClient(); c.baseURL != tc.wantBase {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.wantBase)
			}
		})
	}
}

// llama.cpp response shapes below are captured from the running brain
// engine (27B Q4_K_M, port 11434), not invented.

func testClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

// TestModelPSLlamaFallback: Ollama's /api/ps answers 404 on a llama.cpp
// engine; ModelPS must fall back to /v1/models and map its data[].meta
// block onto ModelInfo (this is what feeds the Grafana model_* gauges).
func TestModelPSLlamaFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{
				"id":"/opt/jarvis-engine/models/jarvis-brain.gguf",
				"object":"model",
				"meta":{
					"n_ctx":131072,"n_ctx_train":262144,"n_embd":5120,
					"n_params":27320697856,"size":16453443584,
					"ftype":"Q4_K - Medium"
				}}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	models, err := testClient(ts.URL).ModelPS()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	m := models[0]
	if m.Name != "jarvis-brain.gguf" {
		t.Errorf("Name = %q, want jarvis-brain.gguf", m.Name)
	}
	if m.Model != "/opt/jarvis-engine/models/jarvis-brain.gguf" {
		t.Errorf("Model = %q", m.Model)
	}
	if m.Size != 16453443584 {
		t.Errorf("Size = %d, want 16453443584", m.Size)
	}
	if m.ContextLength != 131072 {
		t.Errorf("ContextLength = %d, want 131072", m.ContextLength)
	}
	if got := m.Details.ParameterSize; got != "27.3B" {
		t.Errorf("ParameterSize = %q, want 27.3B", got)
	}
	if got := m.Details.Quantization; got != "Q4_K - Medium" {
		t.Errorf("Quantization = %q, want Q4_K - Medium", got)
	}
}

// TestModelPSOllamaPath: with a real /api/ps the Ollama shape is used,
// no fallback happens.
func TestModelPSOllamaPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"jarvis-brain","model":"/opt/x.gguf","size":123,"context_length":4096}]}`)
		case "/v1/models":
			t.Error("fallback hit on an Ollama engine")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	models, err := testClient(ts.URL).ModelPS()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "jarvis-brain" {
		t.Fatalf("got %+v", models)
	}
	if models[0].Size != 123 || models[0].ContextLength != 4096 {
		t.Errorf("Ollama fields not mapped: %+v", models[0])
	}
}

// TestGenerateLlamaFallback: /api/generate 404s on llama.cpp; Generate
// must fall back to /v1/completions and map usage/timings onto the
// GenerateResponse shape. Token counts prefer the llama.cpp timings block.
func TestGenerateLlamaFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "/v1/completions":
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion",
				"choices":[{"text":"hello there","index":0,"finish_reason":"stop"}],
				"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6},
				"timings":{"prompt_n":4,"prompt_ms":34.5,"predicted_n":2,"predicted_ms":87.25}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	gr, err := testClient(ts.URL).Generate("jarvis-brain", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if gr.Response != "hello there" {
		t.Errorf("Response = %q, want %q", gr.Response, "hello there")
	}
	if gr.PromptEvalCount != 4 || gr.EvalCount != 2 {
		t.Errorf("token counts = %d/%d, want 4/2", gr.PromptEvalCount, gr.EvalCount)
	}
	if gr.PromptEvalDuration != 34500000 || gr.EvalDuration != 87250000 {
		t.Errorf("durations = %d/%d, want 34500000/87250000", gr.PromptEvalDuration, gr.EvalDuration)
	}
}

// TestUnloadModelLlamaNoop: there is no unload API on a llama.cpp
// engine; UnloadModel must report success (the model stays resident).
func TestUnloadModelLlamaNoop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	if err := testClient(ts.URL).UnloadModel("jarvis-brain"); err != nil {
		t.Errorf("UnloadModel on llama.cpp = %v, want nil", err)
	}
}

// TestHealthcheck: /health is the one endpoint both engines serve.
// 200 is healthy; 415 (llama.cpp's answer to GET /) and 500 are not.
func TestHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusOK, true},
		{http.StatusUnsupportedMediaType, false},
		{http.StatusInternalServerError, false},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		if got := testClient(ts.URL).Healthcheck(); got != tc.want {
			t.Errorf("Healthcheck(status %d) = %v, want %v", tc.code, got, tc.want)
		}
		ts.Close()
	}
}

// TestIsOllama: /api/ps 200 means Ollama; 404 or a dead engine means not.
func TestIsOllama(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if !testClient(ts.URL).IsOllama() {
		t.Error("IsOllama with 200 /api/ps = false, want true")
	}
	ts.Close()

	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	if testClient(ts.URL).IsOllama() {
		t.Error("IsOllama with 404 /api/ps = true, want false")
	}
	if testClient("http://127.0.0.1:1").IsOllama() { // dead engine
		t.Error("IsOllama on a dead engine = true, want false")
	}
}
