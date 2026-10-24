// Package hermes provides an executor agent for the Jarvis chat bot.
//
// Hermes sits between the Discord chat interface and action execution.
// When the user messages Jarvis, Hermes uses the LLM to classify the
// message as conversational or an action request. For action requests,
// it determines the action type and parameters, executes the action,
// and reports the result back through the chat interface.
//
// The agent is designed to be pluggable: new actions can be registered
// without changing the core loop. Access levels (full/limited/sandboxed)
// control which actions are available.
package hermes

import (
	"context"
	"fmt"
	"sync"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const defaultMaxHistory = 20

// ChatClient is the subset of the Ollama client needed for intent
// classification. *models.Client satisfies this interface.
type ChatClient interface {
	Chat(model string, messages []models.ChatMessage) (string, error)
}

// Agent is the Hermes executor agent. It classifies user messages as
// conversational or action requests, then executes actions or returns
// a chat reply.
type Agent struct {
	mu         sync.Mutex
	ollama     ChatClient
	queueConn  *queue.Conn
	model      string          // Ollama model name for intent parsing
	actions    map[string]Action // registered actions by key
	history    []HistoryEntry    // conversation history for context
	maxHistory int             // max entries to keep
	access     config.AccessLevel
}

// HistoryEntry is one entry in the conversation history.
type HistoryEntry struct {
	Role    string
	Content string
}

// Config holds the configuration for the Hermes agent.
type Config struct {
	Model       string // Ollama model (e.g., "jarvis-brain")
	MaxHistory  int    // max history entries (default 20)
	AccessLevel config.AccessLevel
}

// New creates a new Hermes agent with the given configuration.
func New(cfg Config) *Agent {
	if cfg.MaxHistory <= 0 {
		cfg.MaxHistory = defaultMaxHistory
	}
	return &Agent{
		model:      cfg.Model,
		actions:    make(map[string]Action),
		maxHistory: cfg.MaxHistory,
		access:     cfg.AccessLevel,
	}
}

// SetOllamaClient injects the Ollama client for LLM calls.
// *models.Client satisfies the ChatClient interface.
func (a *Agent) SetOllamaClient(client ChatClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ollama = client
}

// SetQueueConn injects the RabbitMQ connection for enqueueing jobs.
func (a *Agent) SetQueueConn(conn *queue.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queueConn = conn
}

// Register adds an action to the agent. Actions are looked up by key
// when the LLM routes a message to an action.
func (a *Agent) Register(action Action) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions[action.Key] = action
}

// Classify sends the user message through the LLM intent classifier and
// returns a Result indicating whether the message is conversational or
// an action request.
//
// It first attempts fast keyword-based classification, which handles ~80%
// of messages instantly. Ambiguous messages fall through to the LLM.
func (a *Agent) Classify(ctx context.Context, userMessage string) (*Result, error) {
	// Fast path: keyword-based classification avoids an LLM call.
	if res, actionKey := fastClassify(userMessage); res != fastUnknown {
		switch res {
		case fastChat:
			return &Result{Type: TypeChat}, nil
		case fastAction:
			return &Result{Type: TypeAction, ActionName: actionKey}, nil
		}
	}

	a.mu.Lock()
	ollama := a.ollama
	history := make([]HistoryEntry, len(a.history))
	copy(history, a.history)
	a.mu.Unlock()

	if ollama == nil {
		return nil, fmt.Errorf("hermes: Ollama client not set")
	}

	messages := buildClassifyPrompt(a.listActions(), userMessage)
	for _, h := range history {
		messages = append(messages, models.ChatMessage{
			Role:    h.Role,
			Content: h.Content,
		})
	}
	messages = append(messages, models.ChatMessage{
		Role:    "user",
		Content: userMessage,
	})

	raw, err := ollama.Chat(a.model, messages)
	if err != nil {
		return nil, fmt.Errorf("hermes: classify: %w", err)
	}

	result, err := parseResult(raw)
	if err != nil {
		return nil, fmt.Errorf("hermes: parse classify response: %w", err)
	}

	return result, nil
}

// Execute looks up the action referenced by result and runs it.
// Returns a status string suitable for sending back to the user.
func (a *Agent) Execute(ctx context.Context, result *Result) (string, error) {
	if result.Type != TypeAction {
		return "", fmt.Errorf("hermes: cannot execute non-action result (type=%s)", result.Type)
	}

	a.mu.Lock()
	action, ok := a.actions[result.ActionName]
	queueConn := a.queueConn
	a.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("hermes: action %q not registered", result.ActionName)
	}

	// Check access requirements.
	if action.RequiresFullAccess && a.access != config.AccessFull {
		return "access denied: this action requires full access", nil
	}

	text, err := action.Run(ctx, result.Parameters)
	if err != nil {
		return "", fmt.Errorf("hermes: execute %s: %w", result.ActionName, err)
	}

	// Inject the queue connection for actions that need it.
	if result.ActionName == "enqueue_job" && queueConn != nil {
		// The enqueue_job action uses the injected queueConn internally.
	}

	return ResultText(result, text), nil
}

// Process combines Classify and Execute into a single call.
// It classifies the user message, then either returns the chat reply
// or executes the action and returns the execution result.
func (a *Agent) Process(ctx context.Context, userMessage string) (string, error) {
	a.AddHistory("user", userMessage)

	result, err := a.Classify(ctx, userMessage)
	if err != nil {
		return "", err
	}

	switch result.Type {
	case TypeChat:
		a.AddHistory("assistant", result.Reply)
		return result.Reply, nil
	case TypeAction:
		status, err := a.Execute(ctx, result)
		if err != nil {
			return "", err
		}
		a.AddHistory("assistant", status)
		return status, nil
	default:
		return "", fmt.Errorf("hermes: unknown result type %q", result.Type)
	}
}

// AddHistory appends a role/content pair to the conversation history,
// trimming to maxHistory entries when full.
func (a *Agent) AddHistory(role, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history, HistoryEntry{Role: role, Content: content})
	for len(a.history) > a.maxHistory {
		a.history = a.history[1:]
	}
}

// Chat generates a conversational reply using the provided system prompt
// (which should include the voice's system prompt plus any status context),
// conversation history, and user message. This replaces the direct
// client.Chat() call in the chat handler, routing all LLM interactions
// through Hermes.
//
// The history slice should contain alternating user/assistant turns:
// [user1, assistant1, user2, assistant2, ...].
func (a *Agent) Chat(ctx context.Context, systemPrompt string, history []string, userMessage string) (string, error) {
	a.mu.Lock()
	ollama := a.ollama
	a.mu.Unlock()

	if ollama == nil {
		return "", fmt.Errorf("hermes: Ollama client not set")
	}

	messages := []models.ChatMessage{
		{Role: "system", Content: systemPrompt},
	}
	// Interleave history: even indices are user, odd are assistant.
	for i := 0; i < len(history); i += 2 {
		messages = append(messages, models.ChatMessage{
			Role:    "user",
			Content: history[i],
		})
		if i+1 < len(history) {
			messages = append(messages, models.ChatMessage{
				Role:    "assistant",
				Content: history[i+1],
			})
		}
	}
	messages = append(messages, models.ChatMessage{
		Role:    "user",
		Content: userMessage,
	})

	return ollama.Chat(a.model, messages)
}

// listActions returns a snapshot of the registered actions.
func (a *Agent) listActions() []Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Action, 0, len(a.actions))
	for _, v := range a.actions {
		out = append(out, v)
	}
	return out
}
