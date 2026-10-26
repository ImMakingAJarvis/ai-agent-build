// jarvis-reply answers a message from Nathan with one chat call to the
// brain, pinned to the reserved chat slot. It is the light reply path for
// the pause flow: instead of starting a full Claude Code session (slow,
// and it pulls a big prompt through the work slot), it sends a short
// system prompt, the running job's ID, title and last output lines, plus
// Nathan's message, and sends the answer with `jarvis-discord notify`.
//
// The request is pinned to the chat slot (llama.cpp id_slot, CHAT_SLOT_ID,
// default 1) so it runs on the slot reserved for Nathan's conversation:
// the build round keeps running on the work slot while he gets an answer.
//
// Time limit 2 minutes; on timeout it says "I'm busy on <job>, I'll
// answer after it" and goes on.
//
// Usage: jarvis-reply [--job ID] [--slot N] [--timeout DUR] MESSAGE
// (MESSAGE may come from stdin when no argument is given.)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

const (
	defaultEngineURL = "http://localhost:11434"
	defaultModel     = "jarvis-brain"
	defaultSlot      = 1 // slot 0 is the work slot; slot 1 is Nathan's
	defaultTimeout   = 2 * time.Minute
	replyMaxTokens   = 512
	lastOutputLines  = 20
)

// discordNotify sends a DM to Nathan. A variable so tests can stub it.
var discordNotify = func(message string) error {
	cmd := exec.Command("jarvis-discord", "notify", message)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// replyRequest is the /v1/chat/completions body. id_slot pins the request
// to a llama.cpp parallel slot (the reserved chat slot).
type replyRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
	IDSlot    int           `json:"id_slot"`
}

type replyResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// confDir is $JARVIS_CONF or ~/.config/jarvis.
func confDir() string {
	if d := os.Getenv("JARVIS_CONF"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return filepath.Join(home, ".config", "jarvis")
}

// readLocalEnv loads KEY=VALUE pairs from local.env (the file that holds
// ENGINE_URL/OLLAMA_URL and LOCAL_MODEL). Missing file is not an error.
func readLocalEnv(dir string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(filepath.Join(dir, "local.env"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// envOr looks up a key in env, then in local.env, then returns def.
func envOr(key, def string, env map[string]string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := env[key]; v != "" {
		return v
	}
	return def
}

// slotFlag returns the chat slot: --slot, then CHAT_SLOT_ID, then default.
func slotFlag(v string) int {
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if v := os.Getenv("CHAT_SLOT_ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultSlot
}

// currentJobID reads the running job's ID from $CONF/current-job.
func currentJobID() string {
	b, err := os.ReadFile(filepath.Join(confDir(), "current-job"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// jobContext looks up a job's title and its last output lines from the
// job store. Missing store or job yields empty strings (the reply still
// works, just without context).
func jobContext(id string) (title, lastOutput string) {
	if id == "" {
		return "", ""
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", ""
	}
	job, err := store.Get(id)
	if err != nil {
		return "", ""
	}
	title = job.Title
	b, err := store.ReadFile(id, "output.txt")
	if err != nil {
		return title, ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > lastOutputLines {
		lines = lines[len(lines)-lastOutputLines:]
	}
	return title, strings.Join(lines, "\n")
}

// buildMessages assembles the single chat call: a short system prompt,
// then the job context (ID, title, last output lines) and Nathan's message.
func buildMessages(jobID, jobTitle, lastOutput, message string) []chatMessage {
	var user strings.Builder
	fmt.Fprintf(&user, "Job: %s", jobID)
	if jobTitle != "" {
		fmt.Fprintf(&user, " — %s", jobTitle)
	}
	if lastOutput != "" {
		fmt.Fprintf(&user, "\n\nLast output:\n%s", lastOutput)
	}
	fmt.Fprintf(&user, "\n\nNathan: %s", message)
	return []chatMessage{
		{Role: "system", Content: "You are Jarvis, a home AI assistant. " +
			"Your owner Nathan just messaged you while you are working. " +
			"Reply directly to his message in one to three short sentences. " +
			"You may mention what you are working on."},
		{Role: "user", Content: user.String()},
	}
}

// busyMessage is sent when the brain does not answer in time.
func busyMessage(jobID string) string {
	if jobID == "" {
		return "I'm busy, I'll answer after"
	}
	return "I'm busy on " + jobID + ", I'll answer after it"
}

// replyToEngine sends one chat call to the engine, pinned to slot idSlot,
// and returns the assistant's text.
func replyToEngine(baseURL, model string, idSlot int, msgs []chatMessage, timeout time.Duration) (string, error) {
	body, err := json.Marshal(replyRequest{
		Model:     model,
		Messages:  msgs,
		Stream:    false,
		MaxTokens: replyMaxTokens,
		IDSlot:    idSlot,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST /v1/chat/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("/v1/chat/completions returned %d: %s", resp.StatusCode, b)
	}

	var rr replyResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(rr.Choices) == 0 {
		return "", fmt.Errorf("response had no choices")
	}
	return rr.Choices[0].Message.Content, nil
}

func main() {
	var (
		jobID   = flag.String("job", "", "job ID (default: $CONF/current-job)")
		slot    = flag.String("slot", "", "chat slot ID (default: CHAT_SLOT_ID or 1)")
		timeout = flag.Duration("timeout", defaultTimeout, "time limit for the reply")
	)
	flag.Parse()

	var message string
	if flag.NArg() > 0 {
		message = strings.TrimSpace(strings.Join(flag.Args(), " "))
	} else {
		b, _ := io.ReadAll(os.Stdin)
		message = strings.TrimSpace(string(b))
	}
	if message == "" {
		fmt.Fprintln(os.Stderr, "jarvis-reply: no message")
		os.Exit(2)
	}

	env := readLocalEnv(confDir())
	baseURL := envOr("ENGINE_URL", envOr("OLLAMA_URL", defaultEngineURL, env), env)
	model := envOr("LOCAL_MODEL", defaultModel, env)
	id := *jobID
	if id == "" {
		id = currentJobID()
	}
	slotID := slotFlag(*slot)
	title, lastOutput := jobContext(id)

	answer, err := replyToEngine(baseURL, model, slotID,
		buildMessages(id, title, lastOutput, message), *timeout)
	if err != nil {
		// Timed out: the brain was busy. Say so and move on (S1.5).
		if errors.Is(err, context.DeadlineExceeded) {
			_ = discordNotify(busyMessage(id))
			return
		}
		_ = discordNotify("Sorry, my brain stumbled; try again in a moment.")
		os.Exit(1)
	}

	if err := discordNotify(answer); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-reply: notify: %v\n", err)
		os.Exit(1)
	}
}
