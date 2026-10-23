package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// mockOllamaClient simulates the Ollama Chat API for tests.
type mockOllamaClient struct {
	mu       sync.Mutex
	responses []string // queue of responses to return
	calls    []models.ChatMessage // records of messages sent
}

func (m *mockOllamaClient) Chat(_ string, messages []models.ChatMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, messages...)
	if len(m.responses) == 0 {
		return "", fmt.Errorf("mock: no responses configured")
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

// chatResponseJSON returns a JSON string for a chat reply.
func chatResponseJSON(reply string) string {
	b, _ := json.Marshal(map[string]string{
		"type":  TypeChat,
		"reply": reply,
	})
	return string(b)
}

// actionResponseJSON returns a JSON string for an action request.
func actionResponseJSON(action string, params map[string]string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"type":   TypeAction,
		"action": action,
		"params": params,
	})
	return string(b)
}

func TestClassify_Conversational(t *testing.T) {
	mock := &mockOllamaClient{
		responses: []string{chatResponseJSON("Hello! How can I help you today?")},
	}

	agent := New(Config{
		Model:       "jarvis-brain",
		MaxHistory:  10,
		AccessLevel: config.AccessFull,
	})
	agent.SetOllamaClient(mock)

	result, err := agent.Classify(context.Background(), "Hey Jarvis, how are you?")
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if result.Type != TypeChat {
		t.Errorf("expected type %q, got %q", TypeChat, result.Type)
	}
	if result.Reply != "Hello! How can I help you today?" {
		t.Errorf("unexpected reply: %q", result.Reply)
	}
}

func TestClassify_ActionRequest(t *testing.T) {
	mock := &mockOllamaClient{
		responses: []string{actionResponseJSON("check_status", nil)},
	}

	agent := New(Config{
		Model:       "jarvis-brain",
		MaxHistory:  10,
		AccessLevel: config.AccessFull,
	})
	agent.SetOllamaClient(mock)

	result, err := agent.Classify(context.Background(), "What is the current status?")
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if result.Type != TypeAction {
		t.Errorf("expected type %q, got %q", TypeAction, result.Type)
	}
	if result.ActionName != "check_status" {
		t.Errorf("expected action %q, got %q", "check_status", result.ActionName)
	}
}

func TestExecute_RegisteredAction(t *testing.T) {
	called := false
	agent := New(Config{
		Model:       "jarvis-brain",
		AccessLevel: config.AccessFull,
	})
	agent.Register(Action{
		Key:         "test_action",
		Description: "A test action",
		Run: func(ctx context.Context, params map[string]string) (string, error) {
			called = true
			return "test ok", nil
		},
	})

	result := &Result{
		Type:       TypeAction,
		ActionName: "test_action",
		Parameters: map[string]string{"key": "value"},
	}

	status, err := agent.Execute(context.Background(), result)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !called {
		t.Error("action Run was not called")
	}
	if status == "" {
		t.Error("expected non-empty status")
	}
}

func TestExecute_UnknownAction(t *testing.T) {
	agent := New(Config{
		Model: "jarvis-brain",
	})

	result := &Result{
		Type:       TypeAction,
		ActionName: "nonexistent_action",
	}

	_, err := agent.Execute(context.Background(), result)
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestExecute_AccessDenied(t *testing.T) {
	agent := New(Config{
		Model:       "jarvis-brain",
		AccessLevel: config.AccessLimited,
	})
	agent.Register(Action{
		Key:              "restricted_action",
		Description:      "Requires full access",
		RequiresFullAccess: true,
		Run: func(ctx context.Context, params map[string]string) (string, error) {
			return "should not reach here", nil
		},
	})

	result := &Result{
		Type:       TypeAction,
		ActionName: "restricted_action",
	}

	status, err := agent.Execute(context.Background(), result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == "" {
		t.Error("expected non-empty denied message")
	}
}

func TestProcess_ChatPassthrough(t *testing.T) {
	mock := &mockOllamaClient{
		responses: []string{chatResponseJSON("That's interesting!")},
	}

	agent := New(Config{
		Model:       "jarvis-brain",
		MaxHistory:  5,
		AccessLevel: config.AccessFull,
	})
	agent.SetOllamaClient(mock)

	reply, err := agent.Process(context.Background(), "Tell me a fun fact")
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	if reply != "That's interesting!" {
		t.Errorf("unexpected reply: %q", reply)
	}
}

func TestProcess_ActionExecution(t *testing.T) {
	mock := &mockOllamaClient{
		responses: []string{actionResponseJSON("check_status", nil)},
	}

	agent := New(Config{
		Model:       "jarvis-brain",
		MaxHistory:  5,
		AccessLevel: config.AccessFull,
	})
	agent.SetOllamaClient(mock)
	for _, a := range DefaultActions() {
		agent.Register(a)
	}

	_, err := agent.Process(context.Background(), "Check the current status")
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
}

func TestHistory_TrimmedToMax(t *testing.T) {
	agent := New(Config{
		Model:      "jarvis-brain",
		MaxHistory: 3,
	})

	for i := 0; i < 5; i++ {
		agent.AddHistory("user", fmt.Sprintf("message %d", i))
	}

	agent.mu.Lock()
	history := agent.history
	agent.mu.Unlock()

	if len(history) != 3 {
		t.Errorf("expected 3 history entries, got %d", len(history))
	}
	// Should have messages 2, 3, 4 (oldest trimmed).
	if history[0].Content != "message 2" {
		t.Errorf("expected first entry to be 'message 2', got %q", history[0].Content)
	}
	if history[2].Content != "message 4" {
		t.Errorf("expected last entry to be 'message 4', got %q", history[2].Content)
	}
}

func TestParseResult_Chat(t *testing.T) {
	raw := `{"type": "chat", "reply": "Hello there!"}`
	result, err := parseResult(raw)
	if err != nil {
		t.Fatalf("parseResult error: %v", err)
	}
	if result.Type != TypeChat {
		t.Errorf("expected type %q, got %q", TypeChat, result.Type)
	}
	if result.Reply != "Hello there!" {
		t.Errorf("unexpected reply: %q", result.Reply)
	}
}

func TestParseResult_Action(t *testing.T) {
	raw := `{"type": "action", "action": "enqueue_job", "params": {"title": "fix login", "instructions": "debug the auth flow"}}`
	result, err := parseResult(raw)
	if err != nil {
		t.Fatalf("parseResult error: %v", err)
	}
	if result.Type != TypeAction {
		t.Errorf("expected type %q, got %q", TypeAction, result.Type)
	}
	if result.ActionName != "enqueue_job" {
		t.Errorf("expected action %q, got %q", "enqueue_job", result.ActionName)
	}
	if result.Parameters["title"] != "fix login" {
		t.Errorf("expected title 'fix login', got %q", result.Parameters["title"])
	}
}

func TestParseResult_MarkdownFenced(t *testing.T) {
	raw := "```json\n{\"type\": \"chat\", \"reply\": \"fenced reply\"}\n```"
	result, err := parseResult(raw)
	if err != nil {
		t.Fatalf("parseResult error: %v", err)
	}
	if result.Type != TypeChat {
		t.Errorf("expected type %q, got %q", TypeChat, result.Type)
	}
	if result.Reply != "fenced reply" {
		t.Errorf("unexpected reply: %q", result.Reply)
	}
}

func TestParseResult_InvalidJSON(t *testing.T) {
	_, err := parseResult("not json at all")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseResult_UnknownType(t *testing.T) {
	_, err := parseResult(`{"type": "unknown"}`)
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestDefaultActions(t *testing.T) {
	actions := DefaultActions()
	if len(actions) != 5 {
		t.Errorf("expected 5 default actions, got %d", len(actions))
	}

	keys := make(map[string]bool)
	for _, a := range actions {
		keys[a.Key] = true
	}
	expected := []string{"enqueue_job", "check_status", "list_jobs", "start_engine", "stop_engine"}
	for _, k := range expected {
		if !keys[k] {
			t.Errorf("missing expected action %q", k)
		}
	}
}

func TestResultText(t *testing.T) {
	result := &Result{
		Type:       TypeAction,
		ActionName: "check_status",
	}
	text := ResultText(result, "engine is running")
	expected := "**action** check_status: engine is running"
	if text != expected {
		t.Errorf("ResultText = %q, want %q", text, expected)
	}
}

func TestNew_DefaultMaxHistory(t *testing.T) {
	agent := New(Config{Model: "jarvis-brain"})
	if agent.maxHistory != defaultMaxHistory {
		t.Errorf("expected maxHistory %d, got %d", defaultMaxHistory, agent.maxHistory)
	}
}
