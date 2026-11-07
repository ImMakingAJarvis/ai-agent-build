package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Harness edits files in a working directory according to a prompt. Aider
// is the production implementation (the harness choice recorded in
// docs/work-board/NOTES.md); tests substitute a fake. The working
// directory is a git worktree the runner owns: the harness must not
// commit — --no-git on Aider, and the runner commits.
type Harness interface {
	Run(ctx context.Context, dir string, prompt string, files []string) error
}

// DefaultAiderBin is where the uv-managed venv installs aider (see the
// C1 note in docs/work-board/NOTES.md): a CPython 3.12 venv, because
// aider does not install on the system's Python 3.14.
const DefaultAiderBin = ".local/share/task-runner/venv/bin/aider"

// Aider drives the installed aider binary against a local
// OpenAI-compatible endpoint (the llama.cpp brain or the iGPU engine).
type Aider struct {
	// Bin is the aider executable; the default expands $HOME.
	Bin string
	// Endpoint is the OpenAI-compatible base URL (http://localhost:11434/v1).
	Endpoint string
	// APIKey is a dummy: the local engines ignore it, aider requires it.
	APIKey string
	// Timeout bounds each edit round-trip (aider's --timeout).
	Timeout time.Duration
	// Log receives progress lines; nil is fine.
	Log *slog.Logger
}

// Run executes one aider chat in dir: the prompt (as a message file) plus
// the card's files that already exist. Files that do not exist are not
// passed — aider errors on missing paths — the prompt names them instead
// and aider creates them.
func (a Aider) Run(ctx context.Context, dir string, prompt string, files []string) error {
	bin := a.Bin
	if bin == "" {
		bin = DefaultAiderBin
	}
	if !filepath.IsAbs(bin) {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("aider: %w", err)
		}
		bin = filepath.Join(home, bin)
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("aider: binary %s: %w", bin, err)
	}
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = "http://localhost:11434/v1"
	}
	key := a.APIKey
	if key == "" {
		key = "sk-local"
	}
	timeout := a.Timeout
	if timeout == 0 {
		timeout = 1800 * time.Second
	}

	// The engine serves the GGUF file path as the model name, not the
	// registry id — ask the endpoint which model it serves.
	name, err := servedModel(ctx, endpoint)
	if err != nil {
		return err
	}

	// Aider's own history files would dirty the worktree; point them at a
	// scratch dir that dies with the run.
	scratch, err := os.MkdirTemp("", "taskrunner-aider-")
	if err != nil {
		return fmt.Errorf("aider: scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)
	msgFile := filepath.Join(scratch, "prompt.md")
	if err := os.WriteFile(msgFile, []byte(prompt), 0o644); err != nil {
		return fmt.Errorf("aider: prompt file: %w", err)
	}

	var present []string
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil && !fi.IsDir() {
			present = append(present, f)
		}
	}

	args := []string{
		"--model", "openai/" + name,
		"--openai-api-base", endpoint,
		"--openai-api-key", key,
		"--yes-always",
		"--no-git",
		"--timeout", strconv.Itoa(int(timeout.Seconds())),
		"--message-file", msgFile,
		"--chat-history-file", filepath.Join(scratch, "chat-history.md"),
		"--input-history-file", filepath.Join(scratch, "input-history"),
	}
	args = append(args, present...)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if a.Log != nil {
		a.Log.Info("aider finished", "dir", dir, "files", present, "model", name, "err", err)
	}
	if err != nil {
		return fmt.Errorf("aider: %v: %s", err, tail(string(out), 2000))
	}
	return nil
}

// servedModel asks an OpenAI-compatible /v1/models endpoint for the model
// it serves. The local engines serve exactly one model; more than one is
// ambiguous and refused (the runner must know which model it is paying
// the box's VRAM for).
func servedModel(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/")+"/models", nil)
	if err != nil {
		return "", fmt.Errorf("aider: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("aider: model list at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("aider: model list body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("aider: model list at %s: HTTP %d", endpoint, resp.StatusCode)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("aider: model list is not JSON: %w", err)
	}
	if len(doc.Data) != 1 {
		return "", fmt.Errorf("aider: endpoint %s serves %d models, want exactly one", endpoint, len(doc.Data))
	}
	if doc.Data[0].ID == "" {
		return "", fmt.Errorf("aider: endpoint %s returned an empty model id", endpoint)
	}
	return doc.Data[0].ID, nil
}

// tail is the last n bytes of s (check output in comments is capped).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
