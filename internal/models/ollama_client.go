// Package models provides an HTTP client for the local model engine.
// The engine speaks either the Ollama API or the llama.cpp (llama-server)
// OpenAI-compatible API; each method detects which at call time, so a
// client keeps working when the engine implementation is swapped.
package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	defaultTimeout   = 120 * time.Second
)

// Client wraps HTTP calls to the local model engine (Ollama or
// llama.cpp).
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates an Ollama client pointing at the local instance.
// Reads ENGINE_URL (the engine's canonical name in the docker stack),
// then OLLAMA_URL; defaults to localhost:11434. Same precedence as
// cmd/jarvis-reply and internal/slots.
func NewClient() *Client {
	base := os.Getenv("ENGINE_URL")
	if base == "" {
		base = os.Getenv("OLLAMA_URL")
	}
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
// On a llama.cpp engine (no /api/generate) it uses the OpenAI-compatible
// /v1/completions endpoint and maps its usage/timings onto the same shape.
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

	switch {
	case resp.StatusCode == http.StatusOK:
		var gr GenerateResponse
		if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
			return nil, fmt.Errorf("decode generate response: %w", err)
		}
		return &gr, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return c.llamaGenerate(model, prompt)
	default:
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/api/generate returned %d: %s", resp.StatusCode, b)
	}
}

// llamaGenerate is the /v1/completions path for llama.cpp engines.
func (c *Client) llamaGenerate(model, prompt string) (*GenerateResponse, error) {
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal completions request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/v1/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /v1/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/v1/completions returned %d: %s", resp.StatusCode, b)
	}

	var cr struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		// timings is a llama.cpp extension (non-streaming only).
		Timings struct {
			PromptN  int     `json:"prompt_n"`
			PromptMs float64 `json:"prompt_ms"`
			PredictN int     `json:"predicted_n"`
			PredictMs float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("decode completions response: %w", err)
	}
	gr := &GenerateResponse{
		PromptEvalCount:    cr.Usage.PromptTokens,
		EvalCount:          cr.Usage.CompletionTokens,
		PromptEvalDuration: int64(cr.Timings.PromptMs * 1e6),
		EvalDuration:       int64(cr.Timings.PredictMs * 1e6),
	}
	if cr.Timings.PromptN > 0 {
		gr.PromptEvalCount = cr.Timings.PromptN
	}
	if cr.Timings.PredictN > 0 {
		gr.EvalCount = cr.Timings.PredictN
	}
	if len(cr.Choices) > 0 {
		gr.Response = cr.Choices[0].Text
	}
	return gr, nil
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
// On a llama.cpp engine there is no unload API — the server is dedicated
// to its one model and keeps it resident — so this is a no-op there.
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
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil // llama.cpp: model stays resident
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("/api/generate (unload) returned %d: %s", resp.StatusCode, b)
	}
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

// ModelPS returns detailed info about currently loaded models: /api/ps on
// Ollama, /v1/models on a llama.cpp engine.
func (c *Client) ModelPS() ([]ModelInfo, error) {
	resp, err := c.http.Get(c.baseURL + "/api/ps")
	if err != nil {
		return nil, fmt.Errorf("GET /api/ps: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			Models []ModelInfo `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode /api/ps: %w", err)
		}
		return out.Models, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return c.llamaModelInfo()
	default:
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/api/ps returned %d: %s", resp.StatusCode, b)
	}
}

// llamaModelInfo reports the model a llama.cpp engine has loaded. The
// server is dedicated to one model, so answering /v1/models at all means
// it is loaded. llama.cpp extends the OpenAI response with a data[].meta
// block carrying the context size, parameter count and file size.
// SizeVRAM stays 0: the API does not report VRAM use (the dashboard's GPU
// memory panel covers that).
func (c *Client) llamaModelInfo() ([]ModelInfo, error) {
	resp, err := c.http.Get(c.baseURL + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/v1/models returned %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NParams int64  `json:"n_params"`
				Size    int64  `json:"size"`
				NCtx    int    `json:"n_ctx"`
				Ftype   string `json:"ftype"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	d := out.Data[0]
	mi := ModelInfo{
		Name:          filepath.Base(d.ID),
		Model:         d.ID,
		Size:          d.Meta.Size,
		ContextLength: d.Meta.NCtx,
	}
	if d.Meta.NParams > 0 {
		mi.Details.ParameterSize = fmt.Sprintf("%.1fB", float64(d.Meta.NParams)/1e9)
	}
	mi.Details.Quantization = d.Meta.Ftype
	return []ModelInfo{mi}, nil
}

// IsOllama reports whether the engine speaks the Ollama API (it has
// /api/ps). llama.cpp answers 404/405. A network error reports false —
// the engine is down, and the healthcheck/poller will surface that.
func (c *Client) IsOllama() bool {
	resp, err := c.http.Get(c.baseURL + "/api/ps")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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

// Healthcheck reports whether the engine is up. Both Ollama and llama.cpp
// serve /health (200 when serving); the bare / endpoint does not (llama.cpp
// answers 415 to a plain GET).
func (c *Client) Healthcheck() bool {
	resp, err := c.http.Get(c.baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
