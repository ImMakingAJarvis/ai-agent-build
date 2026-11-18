package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// The chat daemon runs in a container (docker-compose service `chat`) that
// has no docker socket and no journal access. Host status checks therefore
// go through the three doors the container does have: the engine's HTTP
// API, the host PID namespace (compose `pid: host`) and the mounted host
// dirs. (The pre-container check_status shelled out to `docker inspect`
// and `journalctl` inside the container, which could never work — the
// bot answered every "are you alive?" with "engine status: unknown".)

// AgentRunLoopUp reports whether the host agent-run loop is alive. The
// check only works because the chat service shares the host PID namespace
// (docker-compose `pid: host`) and the image carries procps
// (docker/jarvis-go/Dockerfile) — without either, pgrep cannot see host
// processes and the check always reports "not running".
//
// The pattern is anchored to the loop's main process: the systemd unit
// runs the bash script itself, so its command line is exactly
// "bash /usr/local/bin/agent-run". Unanchored, any unrelated command line
// that merely mentions the path (a worker round editing agent-run, a grep
// in a journal, ...) would count as the loop being up.
func AgentRunLoopUp() bool {
	return exec.Command("pgrep", "-f", "^bash /usr/local/bin/agent-run").Run() == nil
}

// engineHealth reports the brain engine's state for a status reply. When
// the brain is down but the iGPU fallback engine is reachable it says so —
// that is the "brain busy, chat on the fallback" state the bot routes into
// (cmd/jarvis-chat/fallback.go).
func engineHealth(ctx context.Context) string {
	base := engineBaseURL()
	if !engineUp(ctx, base) {
		if fb := os.Getenv("FALLBACK_ENGINE_URL"); fb != "" && engineUp(ctx, fb) {
			return "unreachable (chat running on iGPU fallback)"
		}
		return "unreachable"
	}
	if name := engineModelName(ctx, base); name != "" {
		return "ok (" + name + ")"
	}
	return "ok"
}

// engineBaseURL is the engine under check: ENGINE_URL (the bot's own LLM
// endpoint — compose service name in the container, localhost on the host),
// defaulting to the canonical local port.
func engineBaseURL() string {
	if base := os.Getenv("ENGINE_URL"); base != "" {
		return base
	}
	return "http://127.0.0.1:11434"
}

// engineUp probes the engine's health endpoint. llama.cpp serves /health
// ({"status":"ok"}); Ollama-shaped engines serve /api/tags instead — the
// same try-one-then-the-other convention the LLM client uses for
// /api/generate vs /v1/completions (internal/models). The short timeout
// keeps a wedged engine from hanging the Discord reply.
func engineUp(ctx context.Context, base string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/health", "/api/tags"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

// engineModelName fetches the loaded model's display name from the
// engine's /v1/models (both llama.cpp and Ollama serve it). llama.cpp
// reports the GGUF path — reduce it to the file name without extension
// ("jarvis-brain", not "/opt/jarvis-engine/models/jarvis-brain.gguf").
func engineModelName(ctx context.Context, base string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	for _, m := range out.Models {
		name := m.Name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimSuffix(name, ".gguf")
		if name != "" {
			return name
		}
	}
	return ""
}

// queueSnapshot summarizes the agent job store (the container mounts the
// same dir): the active-state counts plus the title of the running job, so
// a "what are you working on?" reply points at the real current work.
func queueSnapshot() (summary, current string, err error) {
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", "", err
	}
	jobs, err := store.List()
	if err != nil {
		return "", "", err
	}
	counts := map[string]int{}
	for _, j := range jobs {
		switch j.Status {
		case agentjob.Running, agentjob.Ready, agentjob.Parked, agentjob.Waiting, agentjob.Claimed:
			counts[j.Status]++
			if j.Status == agentjob.Running && current == "" {
				current = j.Title
			}
		}
	}
	if len(counts) == 0 {
		return "(empty)", "", nil
	}
	var parts []string
	for _, st := range []string{agentjob.Running, agentjob.Ready, agentjob.Parked, agentjob.Waiting, agentjob.Claimed} {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	return strings.Join(parts, ", "), current, nil
}
