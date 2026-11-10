// Package vlmserver manages one llama.cpp llama-server coprocess for a
// vision model (a GGUF plus its --mmproj projector) and asks it to
// describe images through the OpenAI-compatible chat API.
//
// It exists for the capability workers that own their model process
// (plan §3.5): the image.describe worker (EVB-21) starts the server with
// Load, asks it for a caption/prompt/tags per image in Handle, and hands
// the GPU back with Unload after the idle window. The server is a child
// of the worker process — no separate container to stop, and the VRAM it
// holds is exactly the measured vram_mb of its capability entry while it
// is up.
package vlmserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Description is the structured answer the worker asks the model for: a
// human caption, a text-to-image prompt that would recreate a similar
// image, and short tags.
type Description struct {
	Caption string   `json:"caption"`
	Prompt  string   `json:"prompt"`
	Tags    []string `json:"tags"`
}

// Prompt is what Describe sends with the image. The JSON-only contract
// keeps ParseDescription simple and the per-image artifact small.
const Prompt = "Describe this image. Respond with JSON only, no markdown: " +
	`{"caption": "<1-2 sentence description>", ` +
	`"prompt": "<a text-to-image prompt that would recreate a similar image>", ` +
	`"tags": ["3-6 short lowercase tags"]}`

// Server is one llama-server coprocess plus its describe API.
type Server struct {
	// Binary is the llama-server executable. Required.
	Binary string
	// Model and MMProj are the vision GGUF and its projector. Required.
	Model  string
	MMProj string
	// Port is the listen port inside the worker's own network namespace
	// (the server is never exposed to the host). Default 11510.
	Port int
	// NGL is --n-gpu-layers: 99 (all) for the GPU placement, 0 for the
	// CPU fallback.
	NGL int
	// NoMMProjOffload adds --no-mmproj-offload: keep the vision encoder
	// (mmproj) in host RAM. Without it, a ngl-0 server still loads the
	// projector onto the GPU (bench: 1594 MiB down to 488 MiB for
	// Qwen3-VL-2B) — the CPU fallback should not spend VRAM it did not
	// ask for.
	NoMMProjOffload bool
	// CtxSize is --ctx-size. Descriptions are short; 4096 keeps the KV
	// cache small enough to share the 4090's spare VRAM with the brain.
	// Default 4096.
	CtxSize int
	// Threads is --threads (0 = llama.cpp's default).
	Threads int
	// BaseURL overrides the API root (tests point it at an httptest
	// server without starting a process). Empty = http://127.0.0.1:<Port>.
	BaseURL string
	// HealthTimeout bounds Start's wait for /health. Default 240 s.
	HealthTimeout time.Duration
	// HTTPTimeout bounds one Describe round trip. Default 300 s.
	HTTPTimeout time.Duration
	// Log; nil = the standard logger.
	Log *log.Logger

	mu   sync.Mutex
	cmd  *exec.Cmd
	errb *bytes.Buffer // tail of the server's stderr, for Start failures
}

func (s *Server) log() *log.Logger {
	if s.Log != nil {
		return s.Log
	}
	return log.Default()
}

func (s *Server) base() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "http://127.0.0.1:" + strconv.Itoa(or(s.Port, 11510))
}

func or(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Started reports whether the coprocess is currently running.
func (s *Server) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// Start brings the server up (idempotent: a second call while it is up
// returns nil) and waits for /health. It is safe to call from the
// handler (first request after boot) and from the SDK's Load hook (after
// an idle unload).
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cmd != nil {
		s.mu.Unlock()
		return nil
	}
	if s.BaseURL == "" && (s.Binary == "" || s.Model == "" || s.MMProj == "") {
		s.mu.Unlock()
		return errors.New("vlmserver: Binary, Model and MMProj are required")
	}
	args := []string{
		"--model", s.Model,
		"--mmproj", s.MMProj,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(or(s.Port, 11510)),
		"--ctx-size", strconv.Itoa(or(s.CtxSize, 4096)),
		"--n-gpu-layers", strconv.Itoa(s.NGL),
	}
	if s.NoMMProjOffload {
		args = append(args, "--no-mmproj-offload")
	}
	if s.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(s.Threads))
	}
	// BaseURL mode (tests): the server is someone else's process — wait
	// for its /health, exec nothing.
	s.errb = &bytes.Buffer{}
	var cmd *exec.Cmd
	if s.BaseURL == "" {
		cmd = exec.Command(s.Binary, args...) // inherits the env (LD_LIBRARY_PATH for the CUDA libs)
		cmd.Stderr = s.errb
		cmd.Stdout = io.Discard
		if err := cmd.Start(); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("vlmserver: start %s: %w", s.Binary, err)
		}
	}
	s.cmd = cmd
	s.mu.Unlock()

	timeout := s.HealthTimeout
	if timeout <= 0 {
		timeout = 240 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			s.Stop()
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if s.healthy() {
			s.log().Printf("vlmserver: up (port %d, ngl %d, %s)", or(s.Port, 11510), s.NGL, filepath.Base(s.Model))
			return nil
		}
		// The process died while loading (bad model, OOM): stop waiting.
		s.mu.Lock()
		dead := s.cmd != nil && s.cmd.ProcessState != nil
		s.mu.Unlock()
		if dead {
			tail := s.errb.String()
			if len(tail) > 512 {
				tail = tail[len(tail)-512:]
			}
			s.mu.Lock()
			s.cmd = nil
			s.mu.Unlock()
			return fmt.Errorf("vlmserver: process exited during load: %s", strings.TrimSpace(tail))
		}
	}
	s.Stop()
	tail := s.errb.String()
	if len(tail) > 512 {
		tail = tail[len(tail)-512:]
	}
	return fmt.Errorf("vlmserver: not healthy after %s: %s", timeout, strings.TrimSpace(tail))
}

// Stop terminates the coprocess (idempotent). The SDK calls it from the
// idle watchdog; the handler's next Start brings it back.
func (s *Server) Stop() error {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil || cmd.ProcessState != nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cmd.Process.Signal(os.Interrupt) // llama.cpp shuts down on SIGINT/SIGTERM
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	s.log().Printf("vlmserver: stopped (port %d)", or(s.Port, 11510))
	return nil
}

// healthy probes GET /health with a 3 s bound.
func (s *Server) healthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.base()+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Describe sends one image to the server and returns its parsed
// description. The image is inlined as a base64 data URI (llama.cpp's
// OpenAI endpoint accepts data: URLs); the input ref itself never moves.
func (s *Server) Describe(ctx context.Context, imgPath string) (Description, error) {
	b, err := os.ReadFile(imgPath)
	if err != nil {
		return Description{}, fmt.Errorf("read image: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"model": filepath.Base(s.Model),
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": Prompt},
				{"type": "image_url", "image_url": map[string]string{
					"url": "data:image/" + extMIME(imgPath) + ";base64," + base64.StdEncoding.EncodeToString(b),
				}},
			},
		}},
		"max_tokens":  512,
		"temperature": 0.2,
		"stream":      false,
	})
	if err != nil {
		return Description{}, err
	}
	timeout := s.HTTPTimeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", s.base()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Description{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Description{}, fmt.Errorf("describe %s: %w", filepath.Base(imgPath), err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Description{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Description{}, fmt.Errorf("describe %s: server %d: %.300s", filepath.Base(imgPath), resp.StatusCode, rb)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &out); err != nil || len(out.Choices) == 0 {
		return Description{}, fmt.Errorf("describe %s: unparsable reply: %.200s", filepath.Base(imgPath), rb)
	}
	return ParseDescription(out.Choices[0].Message.Content)
}

// extMIME maps the image extension to its MIME type for the data URI.
func extMIME(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".webp":
		return "webp"
	case ".bmp":
		return "bmp"
	default:
		return "png"
	}
}

var (
	reCaption = regexp.MustCompile(`"caption"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	rePrompt  = regexp.MustCompile(`"prompt"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	reQuoted  = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
)

// lenientParse recovers the fields from a reply that is not valid JSON —
// the usual cause is a small model that runs out of tokens inside the
// tags array (bench: Qwen3-VL-2B looped tags and hit max_tokens). The
// caption is the field the worker needs; prompt and tags are best-effort.
func lenientParse(s string) (Description, error) {
	var d Description
	if m := reCaption.FindStringSubmatch(s); m != nil {
		d.Caption = unquote(m[1])
	}
	if m := rePrompt.FindStringSubmatch(s); m != nil {
		d.Prompt = unquote(m[1])
	}
	if i := strings.Index(s, `"tags"`); i >= 0 {
		if j := strings.Index(s[i:], "["); j >= 0 {
			arr := s[i+j+1:]
			if end := strings.Index(arr, "]"); end >= 0 {
				arr = arr[:end]
			}
			for _, m := range reQuoted.FindAllString(arr, -1) {
				if t := unquote(m); t != "" {
					d.Tags = append(d.Tags, t)
				}
			}
		}
	}
	if strings.TrimSpace(d.Caption) == "" {
		return Description{}, fmt.Errorf("no caption recoverable from model reply: %.120s", s)
	}
	return d, nil
}

// unquote decodes a JSON string literal ("..." with escapes) to its value.
func unquote(lit string) string {
	if v, err := strconv.Unquote(lit); err == nil {
		return v
	}
	return lit
}

// ParseDescription turns the model's reply into a Description. It is
// tolerant of the usual LLM decorations (```json fences, prose around
// the object); a reply that fails strict JSON parsing falls back to a
// lenient field extraction (truncated JSON). The only hard failure is a
// reply with no caption at all.
func ParseDescription(content string) (Description, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return Description{}, errors.New("empty model reply")
	}
	// Strip a markdown fence, if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.Index(s, "\n"); i >= 0 {
			head := strings.TrimSpace(s[:i])
			if head == "" || head == "json" {
				s = s[i+1:]
			}
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	// Keep only the outermost JSON object: models sometimes wrap it in
	// a sentence.
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return lenientParse(s)
	}
	var d Description
	if err := json.Unmarshal([]byte(s[start:end+1]), &d); err == nil && strings.TrimSpace(d.Caption) != "" {
		return d, nil
	}
	// Strict parse failed (truncated JSON, or an empty caption): recover
	// what the model actually wrote.
	return lenientParse(s)
}
