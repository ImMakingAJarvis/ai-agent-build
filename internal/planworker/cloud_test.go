package planworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

func anthropicModel(endpoint string) *registry.Model {
	return &registry.Model{
		ID: "claude-sonnet-5-5", Location: "cloud", Engine: "anthropic",
		Endpoint: endpoint, Thinking: "thinking.budget_tokens:16384",
	}
}

func openAIModel(endpoint string) *registry.Model {
	return &registry.Model{ID: "gpt-x", Location: "cloud", Engine: "openai-compatible", Endpoint: endpoint}
}

func testMessages() []models.ChatMessage {
	return []models.ChatMessage{
		{Role: "system", Content: "You are a planner."},
		{Role: "user", Content: "Plan the epic."},
	}
}

// TestAPICallAnthropic checks the request shape (path, auth headers,
// thinking budget, system messages merged) and the response parse (text
// blocks joined, thinking blocks skipped).
func TestAPICallAnthropic(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotCT = r.Header.Get("content-type")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"content":[
			{"type":"thinking","thinking":"hmm"},
			{"type":"text","text":"# Plan"},
			{"type":"text","text":": step one."}
		]}`)
	}))
	defer srv.Close()

	a := &API{HTTP: srv.Client(), Token: "sk-test"}
	out, err := a.Chat(context.Background(), anthropicModel(srv.URL), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "# Plan: step one." {
		t.Fatalf("out = %q", out)
	}
	if gotPath != "/messages" || gotKey != "sk-test" ||
		gotVersion != "2023-06-01" || gotCT != "application/json" {
		t.Fatalf("path=%q key=%q version=%q ct=%q", gotPath, gotKey, gotVersion, gotCT)
	}
	if gotBody["model"] != "claude-sonnet-5-5" || gotBody["max_tokens"] != float64(16384) {
		t.Fatalf("body = %v", gotBody)
	}
	if gotBody["system"] != "You are a planner." {
		t.Fatalf("system = %v (system messages must merge, not send as a message)", gotBody["system"])
	}
	th, _ := gotBody["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(16384) {
		t.Fatalf("thinking = %v", gotBody["thinking"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v (system must merge into the top-level field)", msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "Plan the epic." {
		t.Fatalf("messages[0] = %v", m0)
	}
}

// TestAPICallAnthropicNoThinking: a model without a thinking budget gets
// no thinking block in the request.
func TestAPICallAnthropicNoThinking(t *testing.T) {
	m := anthropicModel("http://unused")
	m.Thinking = ""
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()
	m.Endpoint = srv.URL

	a := &API{HTTP: srv.Client(), Token: "k"}
	if _, err := a.Chat(context.Background(), m, testMessages()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := gotBody["thinking"]; ok {
		t.Fatalf("thinking block sent for a model with no budget: %v", gotBody["thinking"])
	}
}

// TestAPICallAnthropicError: a non-2xx response fails with the status and
// the server's message.
func TestAPICallAnthropicError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","message":"overloaded"}`)
	}))
	defer srv.Close()

	a := &API{HTTP: srv.Client(), Token: "k"}
	_, err := a.Chat(context.Background(), anthropicModel(srv.URL), testMessages())
	if err == nil || !strings.Contains(err.Error(), "529") ||
		!strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("err = %v, want 529 + server message", err)
	}
}

// TestAPICallOpenAICompatible checks the OpenAI-compatible path: path,
// bearer auth, message shape (system stays a message) and response parse.
func TestAPICallOpenAICompatible(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok plan"}}]}`)
	}))
	defer srv.Close()

	a := &API{HTTP: srv.Client(), Token: "sk-test"}
	out, err := a.Chat(context.Background(), openAIModel(srv.URL), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "ok plan" {
		t.Fatalf("out = %q", out)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer sk-test" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != "gpt-x" || gotBody["max_tokens"] != float64(16384) {
		t.Fatalf("body = %v", gotBody)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "You are a planner." {
		t.Fatalf("messages[0] = %v", m0)
	}
}

// TestAPICallOpenAICompatibleEmpty: a response with no content fails.
func TestAPICallOpenAICompatibleEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	a := &API{HTTP: srv.Client(), Token: "k"}
	_, err := a.Chat(context.Background(), openAIModel(srv.URL), testMessages())
	if err == nil || !strings.Contains(err.Error(), "no content") {
		t.Fatalf("err = %v, want no content", err)
	}
}

// TestAPICallUnknownEngine: a cloud engine the API does not implement
// fails with a clear error before any request.
func TestAPICallUnknownEngine(t *testing.T) {
	m := openAIModel("http://unused")
	m.Engine = "vllm"
	a := &API{Token: "k"}
	_, err := a.Chat(context.Background(), m, testMessages())
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err = %v, want not supported", err)
	}
}
