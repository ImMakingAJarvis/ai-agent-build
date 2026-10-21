// Package models provides an HTTP client for the local Ollama instance.
package models

import (
	"bytes"
	"encoding/json"
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

// Healthcheck sends a trivial request and returns whether Ollama responds.
func (c *Client) Healthcheck() bool {
	_, err := c.http.Get(c.baseURL + "/")
	return err == nil
}
