package planworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// Cloud calls a cloud model (a registry entry with location "cloud").
type Cloud interface {
	// Chat runs one completion against m's endpoint and returns the
	// reply text.
	Chat(ctx context.Context, m *registry.Model, messages []models.ChatMessage) (string, error)
}

// defaultCloudMaxTokens caps one cloud planning call. A plan document plus
// the task list JSON fits well under it; the registry does not declare an
// output cap.
const defaultCloudMaxTokens = 16384

// anthropicVersion is the anthropic-version header for the Messages API.
const anthropicVersion = "2023-06-01"

// API is the Cloud implementation for the registry's cloud engines
// ("anthropic" and "openai-compatible"), selected by the model's engine
// field.
type API struct {
	HTTP *http.Client
	// Token is the API key: ANTHROPIC_API_KEY for anthropic engines,
	// OPENAI_API_KEY for openai-compatible ones.
	Token string
	// MaxTokens caps completions; 0 uses defaultCloudMaxTokens.
	MaxTokens int
}

// NewAPI builds the default cloud caller from the environment.
func NewAPI() *API {
	return &API{
		HTTP:  &http.Client{Timeout: 30 * time.Minute},
		Token: firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("OPENAI_API_KEY")),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// thinkingBudget parses a registry thinking field ("param:value", e.g.
// "thinking.budget_tokens:16384") and returns the budget when param is
// param, 0 otherwise.
func thinkingBudget(thinking, param string) int {
	name, val, ok := strings.Cut(thinking, ":")
	if !ok {
		return 0
	}
	if strings.TrimSpace(name) != param {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(val), "%d", &n); err != nil {
		return 0
	}
	return n
}

func (a *API) Chat(ctx context.Context, m *registry.Model, messages []models.ChatMessage) (string, error) {
	if a == nil {
		return "", fmt.Errorf("no cloud caller configured (model %s is %s)", m.ID, m.Location)
	}
	maxTokens := a.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultCloudMaxTokens
	}
	switch m.Engine {
	case "anthropic":
		return a.anthropic(ctx, m, messages, maxTokens)
	case "openai-compatible":
		return a.openAICompatible(ctx, m, messages, maxTokens)
	default:
		return "", fmt.Errorf("model %s: cloud engine %q not supported", m.ID, m.Engine)
	}
}

// anthropic calls the Anthropic Messages API (POST <endpoint>/messages).
func (a *API) anthropic(ctx context.Context, m *registry.Model, messages []models.ChatMessage, maxTokens int) (string, error) {
	body := map[string]any{
		"model":      m.ID,
		"max_tokens": maxTokens,
		"messages":   make([]map[string]string, 0, len(messages)),
	}
	if b := thinkingBudget(m.Thinking, "thinking.budget_tokens"); b > 0 {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": b}
	}
	for _, msg := range messages {
		if msg.Role == "system" {
			if s, ok := body["system"].(string); ok {
				body["system"] = s + "\n" + msg.Content
			} else {
				body["system"] = msg.Content
			}
			continue
		}
		body["messages"] = append(body["messages"].([]map[string]string), map[string]string{
			"role": msg.Role, "content": msg.Content,
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal anthropic request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(m.Endpoint, "/")+"/messages", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build anthropic request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.Token)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode anthropic response: %w", err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("anthropic response had no text content")
	}
	return b.String(), nil
}

// openAICompatible calls an OpenAI-compatible chat completions endpoint
// (POST <endpoint>/chat/completions).
func (a *API) openAICompatible(ctx context.Context, m *registry.Model, messages []models.ChatMessage, maxTokens int) (string, error) {
	body := map[string]any{
		"model":      m.ID,
		"max_tokens": maxTokens,
		"messages":   make([]map[string]string, 0, len(messages)),
	}
	for _, msg := range messages {
		body["messages"] = append(body["messages"].([]map[string]string), map[string]string{
			"role": msg.Role, "content": msg.Content,
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal openai-compatible request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(m.Endpoint, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build openai-compatible request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+a.Token)

	resp, err := a.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode openai-compatible response: %w", err)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("openai-compatible response had no content")
	}
	return out.Choices[0].Message.Content, nil
}

// do sends req and returns the response body, which the caller must close,
// failing on non-2xx status with the server's message.
func (a *API) do(req *http.Request) (*http.Response, error) {
	httpClient := a.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s returned %d: %s", req.URL.Path, resp.StatusCode, b)
	}
	return resp, nil
}
