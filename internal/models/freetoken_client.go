package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// FreeTokenClient wraps HTTP calls to the FreeToken API server.
// FreeToken exposes an OpenAI-compatible API.
type FreeTokenClient struct {
	baseURL string
	http    *http.Client
}

// NewFreeTokenClient creates a client for the FreeToken server.
// Reads MOE_ENGINE_URL from environment; defaults to localhost:11435.
func NewFreeTokenClient() *FreeTokenClient {
	base := getEnv("MOE_ENGINE_URL", "http://localhost:11435")
	return &FreeTokenClient{
		baseURL: base,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// FreeTokenChatRequest is the body for a FreeToken /v1/chat/completions request.
type FreeTokenChatRequest struct {
	Model    string                   `json:"model"`
	Messages []FreeTokenChatMessage   `json:"messages"`
	Stream   bool                     `json:"stream"`
	MaxTokens *int                    `json:"max_tokens,omitempty"`
	Temperature *float64              `json:"temperature,omitempty"`
	Thinking  *FreeTokenThinkingConfig `json:"thinking,omitempty"`
}

// FreeTokenThinkingConfig controls the thinking (chain-of-thought) mode.
type FreeTokenThinkingConfig struct {
	Type            string `json:"type"`
	BudgetTokens    int    `json:"budget_tokens,omitempty"`
	ExtendThoughts  bool   `json:"extend_thoughts,omitempty"`
}

// FreeTokenChatMessage is one entry in the chat history.
type FreeTokenChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// FreeTokenChatResponse mirrors the relevant fields from /v1/chat/completions.
type FreeTokenChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int    `json:"index"`
		Message struct {
			Role              string `json:"role"`
			Content           string `json:"content"`
			ReasoningContent  string `json:"reasoning_content,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// FreeTokenChat sends a non-streaming chat completion request and returns the
// assistant's reply text. It mirrors the OpenAI /v1/chat/completions API.
func (c *FreeTokenClient) FreeTokenChat(model string, messages []FreeTokenChatMessage) (string, error) {
	req := FreeTokenChatRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal freetoken chat request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /v1/chat/completions (freetoken): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("/v1/chat/completions (freetoken) returned %d: %s", resp.StatusCode, b)
	}

	var cr FreeTokenChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode freetoken chat response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("freetoken chat response had no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// FreeTokenHealthCheck probes the FreeToken server root endpoint.
func (c *FreeTokenClient) FreeTokenHealthCheck() bool {
	resp, err := c.http.Get(c.baseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// FreeTokenVersion returns version info from the FreeToken server.
func (c *FreeTokenClient) FreeTokenVersion() (string, error) {
	resp, err := c.http.Get(c.baseURL + "/version")
	if err != nil {
		return "", fmt.Errorf("GET /version (freetoken): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("/version (freetoken) returned %d: %s", resp.StatusCode, b)
	}

	var version map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return "", fmt.Errorf("decode /version (freetoken): %w", err)
	}
	if v, ok := version["version"].(string); ok {
		return v, nil
	}
	return "unknown", nil
}

// FreeTokenModelInfo returns info about the currently loaded model.
func (c *FreeTokenClient) FreeTokenModelInfo() (map[string]interface{}, error) {
	resp, err := c.http.Get(c.baseURL + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models (freetoken): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/v1/models (freetoken) returned %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /v1/models (freetoken): %w", err)
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	return out.Data[0], nil
}

// FreeTokenGenerateRequest mirrors a simplified generate request.
type FreeTokenGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// FreeTokenGenerate sends a non-streaming generate request via the chat API.
// FreeToken doesn't have a /api/generate endpoint, so we convert the prompt
// to a chat message.
func (c *FreeTokenClient) FreeTokenGenerate(model, prompt string) (string, error) {
	messages := []FreeTokenChatMessage{
		{Role: "user", Content: prompt},
	}
	return c.FreeTokenChat(model, messages)
}

// FreeTokenLoadModel is a no-op for FreeToken — the model is loaded at
// server startup, not on-demand like Ollama.
func (c *FreeTokenClient) FreeTokenLoadModel(model string) error {
	// FreeToken loads the model at startup; no dynamic loading.
	// We verify the server is healthy and the model is available.
	if !c.FreeTokenHealthCheck() {
		return fmt.Errorf("freetoken server is not healthy")
	}
	info, err := c.FreeTokenModelInfo()
	if err != nil {
		return fmt.Errorf("freetoken model info: %w", err)
	}
	if info == nil {
		return fmt.Errorf("freetoken: no model loaded")
	}
	return nil
}

// FreeTokenUnloadModel is a no-op for FreeToken — the model stays loaded
// for the lifetime of the server process.
func (c *FreeTokenClient) FreeTokenUnloadModel(model string) error {
	return nil
}

// FreeTokenListRunning returns info about the loaded model.
type FreeTokenRunningModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

func (c *FreeTokenClient) FreeTokenListRunning() ([]FreeTokenRunningModel, error) {
	resp, err := c.http.Get(c.baseURL + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models (freetoken): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/v1/models (freetoken) returned %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Data []FreeTokenRunningModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /v1/models (freetoken): %w", err)
	}
	return out.Data, nil
}

// FreeTokenModelPS returns detailed info about the FreeToken server state.
type FreeTokenModelPSInfo struct {
	Model       string `json:"model"`
	MaxSeqLen   int    `json:"max_seq_len"`
	MaxRunning  int    `json:"max_running_requests"`
	MoEBackend  string `json:"moe_backend"`
	CacheSize   int    `json:"moe_cache_size"`
}

func (c *FreeTokenClient) FreeTokenModelPS() ([]FreeTokenModelPSInfo, error) {
	resp, err := c.http.Get(c.baseURL + "/health")
	if err != nil {
		return nil, fmt.Errorf("GET /health (freetoken): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("/health (freetoken) returned %d: %s", resp.StatusCode, b)
	}

	var info FreeTokenModelPSInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		// Health endpoint may return a simple string; that's fine.
		return []FreeTokenModelPSInfo{{Model: "loaded"}}, nil
	}
	return []FreeTokenModelPSInfo{info}, nil
}
