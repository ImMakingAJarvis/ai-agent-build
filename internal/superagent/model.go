// Package superagent is the Super Agent: a small autonomous agent loop
// modelled on DeepSeek's super agent (docs/superagent.md). It takes a
// goal, plans the steps, executes them by calling tools (shell, files,
// memory search, web fetch), verifies each step, and reports a result —
// without per-step human input. It runs on the local brain engine via the
// OpenAI-compatible /v1/chat/completions endpoint.
package superagent

import (
	"context"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Model answers one chat completion. OllamaModel is the production
// implementation; tests use a scripted fake.
type Model interface {
	Chat(ctx context.Context, messages []Message) (string, error)
}

// OllamaModel adapts the shared models.Client (llama.cpp or Ollama) to
// Model, pinned to one model name.
type OllamaModel struct {
	C     *models.Client
	Model string
}

// NewOllamaModel builds a Model for model on the engine at baseURL.
func NewOllamaModel(baseURL, model string) *OllamaModel {
	return &OllamaModel{C: models.NewClientWithBase(baseURL), Model: model}
}

// Chat runs one completion against the pinned model.
func (m *OllamaModel) Chat(ctx context.Context, messages []Message) (string, error) {
	msgs := make([]models.ChatMessage, len(messages))
	for i, msg := range messages {
		msgs[i] = models.ChatMessage{Role: msg.Role, Content: msg.Content}
	}
	return m.C.Chat(m.Model, msgs)
}
