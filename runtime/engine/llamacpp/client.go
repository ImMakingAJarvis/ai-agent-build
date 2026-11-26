// Package llamacpp is the llama.cpp backend for the engine.Engine
// interface (ELR-1, M2.1b, card ELR-18). This file covers the non-chat
// half: health, model info, slot state, and KV-cache snapshot
// save/restore/erase over the llama-server HTTP API. The chat stream
// (M2.1c) will add the Chat method.
//
// The request shapes copy what internal/slots and internal/agentjob
// already do against the same API (read those before changing this
// file); the reply field names were verified against the box's engine
// (see testdata/props.json and testdata/models.json).
package llamacpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// defaultSlotDir is the host directory the engine writes slot
// snapshots into; the client only stats files there.
const defaultSlotDir = "/var/lib/jarvis/slots"

// Client talks to one llama-server. BaseURL has no trailing slash.
// SlotDir is the host directory the engine saves snapshots into; it is
// used only to stat a saved file (the engine itself is what writes it).
type Client struct {
	BaseURL string
	HTTP    *http.Client
	SlotDir string
}

// New creates a Client for baseURL with a 10s client timeout and the
// default slot snapshot directory. Callers that need different limits
// may override the HTTP or SlotDir fields.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		SlotDir: defaultSlotDir,
	}
}

// Health returns nil only when GET /health answers 200.
func (c *Client) Health(ctx context.Context) error {
	return c.get(ctx, "/health", nil)
}

// Slots reports the state of every engine slot.
func (c *Client) Slots(ctx context.Context) ([]engine.SlotState, error) {
	var raw []struct {
		ID            int  `json:"id"`
		IsProcessing  bool `json:"is_processing"`
		NPromptTokens int  `json:"n_prompt_tokens"`
	}
	if err := c.get(ctx, "/slots", &raw); err != nil {
		return nil, err
	}
	out := make([]engine.SlotState, len(raw))
	for i, s := range raw {
		out[i] = engine.SlotState{ID: s.ID, Busy: s.IsProcessing, Tokens: s.NPromptTokens}
	}
	return out, nil
}

// Info reports the loaded model and the engine build, reading GET
// /props and GET /v1/models.
func (c *Client) Info(ctx context.Context) (engine.ModelInfo, error) {
	var props struct {
		ModelAlias string `json:"model_alias"`
		ModelPath  string `json:"model_path"`
		BuildInfo  string `json:"build_info"`
		DefaultGenerationSettings struct {
			Params struct {
				NCtx int `json:"n_ctx"`
			} `json:"params"`
		} `json:"default_generation_settings"`
	}
	if err := c.get(ctx, "/props", &props); err != nil {
		return engine.ModelInfo{}, err
	}
	var models struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int   `json:"n_ctx"`
				Size int64 `json:"size"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/v1/models", &models); err != nil {
		return engine.ModelInfo{}, err
	}
	info := engine.ModelInfo{
		ID:            props.ModelAlias,
		ContextTokens: props.DefaultGenerationSettings.Params.NCtx,
		// KVType is not reported by /props or /v1/models; left empty.
		Build:     props.BuildInfo,
		ModelFile: props.ModelPath,
	}
	if len(models.Data) > 0 {
		if id := models.Data[0].ID; id != "" {
			info.ID = id
		}
		if info.ContextTokens == 0 {
			info.ContextTokens = models.Data[0].Meta.NCtx
		}
		info.ModelBytes = models.Data[0].Meta.Size
	}
	return info, nil
}

// Caps reports the optional behaviours this backend supports.
func (c *Client) Caps() engine.Caps {
	return engine.Caps{SlotSave: true}
}

// SaveSlot writes slot's KV cache to a snapshot named name and reports
// what was saved. The engine writes the file itself; the client only
// times the request and stats the resulting file for its size.
func (c *Client) SaveSlot(ctx context.Context, slot int, name string) (engine.SnapshotMeta, error) {
	if err := checkSnapshotName(name); err != nil {
		return engine.SnapshotMeta{}, err
	}
	start := time.Now()
	var reply struct {
		NSaved int `json:"n_saved"`
	}
	if err := c.slotAction(ctx, slot, "save", map[string]string{"filename": name}, &reply); err != nil {
		return engine.SnapshotMeta{}, err
	}
	meta := engine.SnapshotMeta{
		File:    name,
		Tokens:  reply.NSaved,
		Seconds: time.Since(start).Seconds(),
	}
	if c.SlotDir != "" {
		if fi, err := os.Stat(filepath.Join(c.SlotDir, name)); err == nil {
			meta.Bytes = fi.Size()
		}
	}
	return meta, nil
}

// RestoreSlot loads the snapshot named name into slot.
func (c *Client) RestoreSlot(ctx context.Context, slot int, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	return c.slotAction(ctx, slot, "restore", map[string]string{"filename": name}, nil)
}

// EraseSlot drops slot's KV cache.
func (c *Client) EraseSlot(ctx context.Context, slot int) error {
	return c.slotAction(ctx, slot, "erase", nil, nil)
}

// checkSnapshotName refuses names that are empty or that could escape
// the snapshot directory, before any request is made.
func checkSnapshotName(name string) error {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("bad snapshot name %q", name)
	}
	return nil
}

// slotAction calls /slots/<slot>?action=action with an optional JSON
// body and decodes any reply into out.
func (c *Client) slotAction(ctx context.Context, slot int, action string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s body: %w", action, err)
		}
		reader = bytes.NewReader(b)
	}
	u := fmt.Sprintf("/slots/%d?%s", slot, url.Values{"action": {action}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+u, reader)
	if err != nil {
		return fmt.Errorf("build %s request: %w", action, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slot %s: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return httpError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode slot %s reply: %w", action, err)
		}
	}
	return nil
}

// get performs GET path and decodes the 200 reply into out (which may
// be nil). Any status above 299 is an error carrying the status and a
// body prefix.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return httpError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// httpError describes a non-2xx reply: the status and the first 200
// bytes of the body.
func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("%s %s: HTTP %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body)
}
