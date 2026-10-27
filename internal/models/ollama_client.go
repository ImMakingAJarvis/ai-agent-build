// Package models provides an HTTP client for the local Ollama instance.
package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	defaultTimeout   = 120 * time.Second
)

// Client wraps HTTP calls to the Ollama API.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates an Ollama client pointing at the local instance.
// Reads OLLAMA_URL from environment; defaults to localhost:11434.
func NewClient() *Client {
	base := os.Getenv("OLLAMA_URL")
	if base == "" {
		base = defaultOllamaURL
	}
	return &Client{
		baseURL: base,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// GenerateRequest mirrors the Ollama /api/generate request body.
type GenerateRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Stream    bool   `json:"stream"`
	KeepAlive string `json:"keep_alive,omitempty"`
}

// GenerateResponse mirrors the relevant fields from Ollama /api/generate.
type GenerateResponse struct {
	Model              string `json:"model"`
	Response           string `json:"response"`
	Done               bool   `json:"done"`
	PromptEvalCount    int    `json:"prompt_eval_count"`
	EvalCount          int    `json:"eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalDuration       int64  `json:"eval_duration"`
}

// Generate sends a non-streaming generate request and returns the full response.
func (c *Client) Generate(model, prompt string) (*GenerateResponse, error) {
	req := GenerateRequest{
		Model:  model,
		Prompt: prompt,
		Stream: false,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal generate request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /api/generate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/api/generate returned %d: %s", resp.StatusCode, b)
	}

	var gr GenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("decode generate response: %w", err)
	}
	return &gr, nil
}

// LoadModel loads a model into Ollama's memory. keepAlive controls how long it stays loaded.
// keepAlive="" means the Ollama default; keepAlive="-1" means forever.
func (c *Client) LoadModel(model, keepAlive string) error {
	req := map[string]string{"name": model}
	if keepAlive != "" {
		req["keep_alive"] = keepAlive
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal load request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/api/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /api/pull: %w", err)
	}
	defer resp.Body.Close()

	// /api/pull returns 200 even if already present.
	// We also call /api/generate with an empty prompt to ensure it's loaded.
	_, _ = c.Generate(model, "")
	return nil
}

// UnloadModel tells Ollama to unload a model by setting keep_alive to 0.
func (c *Client) UnloadModel(model string) error {
	req := GenerateRequest{
		Model:     model,
		Prompt:    "",
		Stream:    false,
		KeepAlive: "0",
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal unload request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /api/generate (unload): %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// ListRunning returns the models currently loaded in Ollama memory.
type RunningModel struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
	ExpiresAt string `json:"expires_at"`
}

func (c *Client) ListRunning() ([]RunningModel, error) {
	resp, err := c.http.Get(c.baseURL+"/api/tags")
	if err != nil {
		return nil, fmt.Errorf("GET /api/tags: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/api/tags returned %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Models []RunningModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /api/tags: %w", err)
	}
	return out.Models, nil
}

// ModelInfo holds detailed information about a loaded model from /api/ps.
type ModelInfo struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Size          int64  `json:"size"`
	SizeVRAM      int64  `json:"size_vram"`
	ContextLength int    `json:"context_length"`
	Runner        string `json:"runner"`
	Details       struct {
		Family        string `json:"family"`
		ParameterSize string `json:"parameter_size"`
		Quantization  string `json:"quantization_level"`
	} `json:"details"`
}

// ModelPS queries /api/ps and returns detailed info about currently loaded models.
func (c *Client) ModelPS() ([]ModelInfo, error) {
	resp, err := c.http.Get(c.baseURL + "/api/ps")
	if err != nil {
		return nil, fmt.Errorf("GET /api/ps: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/api/ps returned %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /api/ps: %w", err)
	}
	return out.Models, nil
}

// ChatRequest is the body for a /v1/chat/completions request.
// Only model, messages, stream and think are sent — extra options
// (num_ctx etc.) force Ollama to reload the 17 GB model.
type ChatRequest struct {
	Model              string         `json:"model"`
	Messages           []ChatMessage  `json:"messages"`
	Stream             bool           `json:"stream"`
	Think              bool           `json:"think"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

// ChatMessage is one entry in the chat history.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse mirrors the relevant fields from /v1/chat/completions.
type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

// Chat sends a non-streaming chat completion request and returns the
// assistant's reply text. It only sends model, messages, stream: false
// and think: false to avoid forcing Ollama to reload the model.
func (c *Client) Chat(model string, messages []ChatMessage) (string, error) {
	req := ChatRequest{
		Model:              model,
		Messages:           messages,
		Stream:             false,
		Think:              false,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /v1/chat/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("/v1/chat/completions returned %d: %s", resp.StatusCode, b)
	}

	var cr ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("chat response had no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// Healthcheck sends a trivial request and returns whether Ollama responds.
func (c *Client) Healthcheck() bool {
	_, err := c.http.Get(c.baseURL + "/")
	return err == nil
}
