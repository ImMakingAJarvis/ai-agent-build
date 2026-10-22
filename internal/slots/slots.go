// Package slots queries the llama.cpp /slots endpoint to report
// the state of each parallel slot (idle, processing, token counts).
package slots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const defaultEngineURL = "http://localhost:11434"

// SlotInfo describes one llama.cpp parallel slot.
type SlotInfo struct {
	ID            int    `json:"id"`
	IsProcessing  bool   `json:"is_processing"`
	TaskID        int    `json:"id_task"`
	NContext      int    `json:"n_ctx"`
	NPromptTokens int    `json:"n_prompt_tokens"`
}

// GetSlots queries the engine's /slots endpoint and returns the slot list.
// When engineURL is empty it falls back to the ENGINE_URL env var, then
// to the default localhost:11434.
func GetSlots(engineURL string) ([]SlotInfo, error) {
	if engineURL == "" {
		engineURL = os.Getenv("ENGINE_URL")
	}
	if engineURL == "" {
		engineURL = defaultEngineURL
	}
	resp, err := http.Get(engineURL + "/slots")
	if err != nil {
		return nil, fmt.Errorf("get /slots: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/slots returned %d", resp.StatusCode)
	}
	var slots []SlotInfo
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return nil, fmt.Errorf("decode /slots: %w", err)
	}
	return slots, nil
}

// Client wraps the engine URL so tests can swap in a test server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient creates a Client from the ENGINE_URL env var or the default.
func NewClient() *Client {
	base := os.Getenv("ENGINE_URL")
	if base == "" {
		base = defaultEngineURL
	}
	return &Client{
		BaseURL: base,
		HTTP: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetSlots queries this client's engine.
func (c *Client) GetSlots() ([]SlotInfo, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/slots")
	if err != nil {
		return nil, fmt.Errorf("get /slots: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/slots returned %d", resp.StatusCode)
	}
	var slots []SlotInfo
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return nil, fmt.Errorf("decode /slots: %w", err)
	}
	return slots, nil
}
