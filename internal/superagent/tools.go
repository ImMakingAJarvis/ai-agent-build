package superagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultShellTimeout = 120 * time.Second
	maxShellTimeout     = 600 * time.Second
	maxReadLines        = 400
	defaultReadLines    = 200
	maxOutput           = 4000 // observation size cap, protects the context
	maxFetchBody        = 3000
)

// ToolSpec is one tool's name and one-line description, for the prompt.
type ToolSpec struct {
	Name        string
	Description string
}

// Specs lists the tools the executor can call, in prompt order.
func Specs() []ToolSpec {
	return []ToolSpec{
		{"shell", `{"cmd": "...", "timeout": 120} — run a command in the work dir (seconds, max 600)`},
		{"read_file", `{"path": "...", "offset": 1, "limit": 200} — read a file, 1-based line offset`},
		{"write_file", `{"path": "...", "content": "..."} — create/overwrite a file in the work dir`},
		{"list_dir", `{"path": "."} — list a directory`},
		{"memory_search", `{"query": "...", "k": 5} — search the memory graph of past work`},
		{"web_fetch", `{"url": "..."} — fetch a URL and return the first part of the body`},
		{"step_done", `{"note": "..."} — this step is achieved; note how it was verified`},
		{"replan", `{"reason": "..."} — the plan no longer fits; the planner runs again`},
		{"finish", `{"summary": "...", "ok": true} — the goal is done (ok) or failed (!ok)`},
	}
}

// Tools executes the file- and shell-tools. Control tools (step_done,
// replan, finish) are handled by the agent loop.
type Tools struct {
	Guard     *Guard
	MemoryURL string // jarvis-memory base URL; "" disables memory_search
	HTTP      *http.Client
}

// NewTools builds the default toolset for workdir.
func NewTools(workdir string) *Tools {
	return &Tools{
		Guard:  &Guard{WorkDir: workdir},
		MemoryURL: "http://localhost:8101",
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Run executes one tool action and returns the observation the model sees.
func (t *Tools) Run(ctx context.Context, a Action) (string, error) {
	switch a.Tool {
	case "shell":
		return t.runShell(ctx, a)
	case "read_file":
		return t.runRead(a)
	case "write_file":
		return t.runWrite(a)
	case "list_dir":
		return t.runList(a)
	case "memory_search":
		return t.runMemory(ctx, a)
	case "web_fetch":
		return t.runFetch(ctx, a)
	default:
		return "", fmt.Errorf("unknown tool %q", a.Tool)
	}
}

func (t *Tools) runShell(ctx context.Context, a Action) (string, error) {
	cmd := argStr(a.Args, "cmd", "")
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("shell needs a cmd")
	}
	if reason, blocked := t.Guard.CheckCommand(cmd); blocked {
		return "BLOCKED: " + reason, nil
	}
	timeout := defaultShellTimeout
	if ms := time.Duration(argInt(a.Args, "timeout", int(defaultShellTimeout.Seconds()))) * time.Second; ms > 0 {
		timeout = ms
		if timeout > maxShellTimeout {
			timeout = maxShellTimeout
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = t.Guard.WorkDir
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	obs := truncate(out.String(), maxOutput)
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("TIMED OUT after %s\n%s", timeout, obs), nil
	case err != nil:
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Sprintf("exit %d\n%s", ee.ExitCode(), obs), nil
		}
		return "", err
	default:
		return obs, nil
	}
}

func (t *Tools) runRead(a Action) (string, error) {
	path := argStr(a.Args, "path", "")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	off := argInt(a.Args, "offset", 1)
	if off < 1 {
		off = 1
	}
	limit := argInt(a.Args, "limit", defaultReadLines)
	if limit < 1 || limit > maxReadLines {
		limit = defaultReadLines
	}
	var sb strings.Builder
	end := off + limit - 1
	for i := off - 1; i < len(lines) && i < end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, lines[i])
	}
	if end > len(lines) {
		fmt.Fprintf(&sb, "(file has %d lines)", len(lines))
	}
	return truncate(sb.String(), maxOutput), nil
}

func (t *Tools) runWrite(a Action) (string, error) {
	path := argStr(a.Args, "path", "")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	content := argStr(a.Args, "content", "")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), filepath.Base(abs)), nil
}

func (t *Tools) runList(a Action) (string, error) {
	path := argStr(a.Args, "path", ".")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	return truncate(strings.Join(names, "\n"), maxOutput), nil
}

func (t *Tools) runMemory(ctx context.Context, a Action) (string, error) {
	if t.MemoryURL == "" {
		return "memory search is disabled", nil
	}
	query := argStr(a.Args, "query", "")
	if query == "" {
		return "", fmt.Errorf("memory_search needs a query")
	}
	k := argInt(a.Args, "k", 5)
	if k < 1 || k > 10 {
		k = 5
	}
	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.MemoryURL+"/search", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "memory service unreachable: " + err.Error(), nil
	}
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			Title string  `json:"title"`
			Text  string  `json:"text"`
			Score float64 `json:"score"`
			Path  string  `json:"path"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode memory search: %w", err)
	}
	if len(out.Results) == 0 {
		return "(no results)", nil
	}
	var sb strings.Builder
	for i, r := range out.Results {
		fmt.Fprintf(&sb, "%d. %s (score %.2f, %s)\n   %s\n", i+1, r.Title, r.Score, r.Path, truncate(strings.TrimSpace(r.Text), 240))
	}
	return truncate(sb.String(), maxOutput), nil
}

func (t *Tools) runFetch(ctx context.Context, a Action) (string, error) {
	url := argStr(a.Args, "url", "")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("web_fetch needs an http(s) url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "fetch failed: " + err.Error(), nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBody+1024))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, truncate(string(b), maxFetchBody)), nil
}

// truncate caps s at n chars, keeping the head and tail (errors and
// results usually sit at the end of command output).
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		if s == "" {
			return "(no output)"
		}
		return s
	}
	head, tail := n*5/8, n/8
	return s[:head] + fmt.Sprintf("\n… [%d chars truncated] …\n", len(s)-head-tail) + s[len(s)-tail:]
}
