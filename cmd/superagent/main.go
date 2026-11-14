// Command superagent runs one Super Agent (internal/superagent) against
// the local brain engine: it takes a goal, plans the steps, executes them
// with the guarded tools (shell, files, memory search, web fetch),
// verifies, and reports. Exit 0 means the agent completed the goal;
// exit 1 means it could not.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/superagent"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		promptFile = flag.String("prompt-file", "", "read the goal from this file (overrides the goal argument)")
		jobID      = flag.String("job", "", "load the goal (title + instructions) from this queued job id")
		model      = flag.String("model", envOr("LOCAL_MODEL", "jarvis-brain"), "model name on the engine")
		engine     = flag.String("engine", envOr("ENGINE_URL", envOr("OLLAMA_URL", "http://localhost:11434")), "engine base URL (OpenAI-compatible /v1/chat/completions)")
		workdir    = flag.String("workdir", "", "working directory for the agent's tools (default: current directory)")
		maxSteps   = flag.Int("max-steps", 0, "step budget across all plans (default 20)")
		maxTurns   = flag.Int("max-turns", 0, "model calls per step (default 8)")
		replans    = flag.Int("replans", -1, "replans allowed (default 2; 0 disables)")
		runTimeout = flag.Duration("run-timeout", 0, "whole-run deadline (default 30m)")
		memoryURL  = flag.String("memory-url", "http://localhost:8101", "jarvis-memory base URL (empty disables memory_search)")
		noGuard    = flag.Bool("no-guard", false, "disable the safety guard (NOT recommended)")
	)
	flag.Parse()

	goal, err := goalFrom(*jobID, *promptFile, flag.Args())
	if err != nil {
		fatal(err)
	}
	if strings.TrimSpace(goal) == "" {
		fatal(fmt.Errorf("empty goal"))
	}

	wd := *workdir
	if wd == "" {
		wd, err = os.Getwd()
		if err != nil {
			fatal(err)
		}
	}
	if wd, err = filepath.Abs(wd); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		fatal(err)
	}

	tools := superagent.NewTools(wd)
	tools.MemoryURL = *memoryURL
	if *noGuard {
		tools.Guard.Disabled = true
	}

	a := superagent.New(superagent.NewOllamaModel(*engine, *model), tools)
	a.Log = os.Stdout
	if *maxSteps > 0 {
		a.MaxSteps = *maxSteps
	}
	if *maxTurns > 0 {
		a.MaxTurns = *maxTurns
	}
	if *replans >= 0 {
		a.Replans = *replans
	}
	if *runTimeout > 0 {
		a.RunTimeout = *runTimeout
	}

	res, err := a.Run(context.Background(), goal)
	if err != nil {
		fatal(fmt.Errorf("run: %w", err))
	}
	word := "FAILED"
	if res.OK {
		word = "DONE"
	}
	if res.Yielded {
		word = "YIELDED"
	}
	fmt.Printf("\nsuperagent: %s in %s (%d steps, %d model calls)\nsummary: %s\n",
		word, res.Duration.Round(time.Second), res.Steps, res.Turns, res.Summary)
	// A yield is not a failure: the run is parked (the job is Waiting, the
	// switchboard's wake is the continuation). Exit 0 so the agentjob
	// runner's finish() leaves the job Waiting rather than marking it done.
	if !res.OK && !res.Yielded {
		os.Exit(1)
	}
}

// goalFrom resolves the goal: --job wins, then --prompt-file, then the
// positional argument(s).
func goalFrom(jobID, promptFile string, args []string) (string, error) {
	switch {
	case jobID != "":
		st, err := agentjob.Open(agentjob.DataDir())
		if err != nil {
			return "", err
		}
		j, err := st.Get(jobID)
		if err != nil {
			return "", err
		}
		return j.Title + "\n\n" + j.Instructions, nil
	case promptFile != "":
		b, err := os.ReadFile(promptFile)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case len(args) > 0:
		return strings.Join(args, " "), nil
	default:
		return "", fmt.Errorf("usage: superagent [goal...] | --job <id> | --prompt-file <file>")
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "superagent:", err)
	os.Exit(1)
}
