package agentjob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

var prRe = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// Runner runs claimed jobs and records the result.
type Runner struct {
	Store *Store
	History *queue.History
	// Command builds the agent command for a driver, full prompt, and reasoning
	// effort level. The default is agent-local (local) or claude --model $AGENT_STAGE2_MODEL.
	Command func(driver, prompt, effort string) *exec.Cmd
	// PRLookup finds a PR URL for a branch when the output has none. The
	// default asks gh. Return "" if there is none.
	PRLookup func(branch string) string
	// Brief returns the stage-3 rules for a driver. The default reads
	// $JARVIS_CONF/stage3-<driver>.txt.
	Brief func(driver string) string
	// Tee, if set, also receives the agent's output live (agent.log).
	Tee io.Writer
	// SignalFile, if non-empty, is polled during job execution. When it
	// appears (written by the Discord watcher on an incoming user message),
	// the running command is cancelled and the job is paused so the agent
	// can answer the user immediately.
	SignalFile string
	// ReplySignalFile, if non-empty, is written by signalWatcher when a
	// Discord message arrives but the engine slot is free — the round is NOT
	// paused. After the job completes, agent-run checks for this file and
	// sends a light reply before continuing.
	ReplySignalFile string
	// EngineSlotCheck reports whether the engine's chat slot is currently
	// busy. When true, an incoming message causes a pause. When false, the
	// round keeps running and a reply is deferred until it completes.
	// The default (when nil) always returns true (slot busy).
	EngineSlotCheck func() bool
	// EngineURL is the base URL of the llama.cpp engine (e.g.
	// http://localhost:11434). When set, pause saves the work slot's KV
	// cache and resume restores it. When empty (Ollama/vLLM), save/restore
	// are no-ops.
	EngineURL string
	// WorkSlotID is the llama.cpp slot ID used for work jobs (default 0).
	WorkSlotID int
}

// ModelFor names the model a driver uses.
func ModelFor(driver string) string {
	if driver == "sonnet" {
		if m := os.Getenv("AGENT_STAGE2_MODEL"); m != "" {
			return m
		}
		return "sonnet"
	}
	if m := os.Getenv("LOCAL_MODEL"); m != "" {
		return m
	}
	return "jarvis-brain"
}

func defaultCommand(driver, prompt, effort string) *exec.Cmd {
	if driver == "sonnet" {
		return exec.Command("claude", "--model", ModelFor(driver),
			"--dangerously-skip-permissions",
			"--reasoning-effort", effort,
			"-p", prompt)
	}
	return exec.Command("agent-local",
		"--dangerously-skip-permissions",
		"--reasoning-effort", effort,
		"-p", prompt)
}

func defaultPRLookup(branch string) string {
	tok, err := exec.Command("ghapp-token").Output()
	if err != nil {
		return ""
	}
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--state", "all", "--json", "url", "--jq", ".[0].url")
	cmd.Env = append(os.Environ(), "GH_TOKEN="+strings.TrimSpace(string(tok)))
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (r *Runner) brief(driver string) string {
	if r.Brief != nil {
		return r.Brief(driver)
	}
	conf := os.Getenv("JARVIS_CONF")
	if conf == "" {
		home, _ := os.UserHomeDir()
		conf = home + "/.config/jarvis"
	}
	b, _ := os.ReadFile(fmt.Sprintf("%s/stage3-%s.txt", conf, driver))
	return string(b)
}

// Prompt is the stage-3 rules plus the job text (and any typed note).
func (r *Runner) Prompt(j Job, driver, note string) string {
	var b strings.Builder
	b.WriteString(r.brief(driver))
	fmt.Fprintf(&b, "\n\n=== YOUR JOB THIS ROUND: %s — %s ===\n", j.ID, j.Title)
	if j.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s\n", j.Branch)
	}
	b.WriteString(j.Instructions)
	if note != "" {
		fmt.Fprintf(&b, "\n\nInstructions from me for this round (they take priority): %s\n", note)
	}
	return b.String()
}

// Summary is the tail of the output, trimmed for the history and dashboard.
func Summary(out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 600 {
		out = "…" + out[len(out)-600:]
	}
	return out
}

// saveSlot saves the work slot's KV cache to disk via the llama.cpp
// /slots/<id>?action=save API. No-op when EngineURL is empty (Ollama/vLLM).
func (r *Runner) saveSlot(jobID string) error {
	if r.EngineURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"filename": jobID + ".bin"})
	resp, err := http.Post(r.EngineURL+"/slots/"+fmt.Sprint(r.WorkSlotID)+"?action=save",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return fmt.Errorf("save slot %d: HTTP %d", r.WorkSlotID, resp.StatusCode)
	}
	return nil
}

// restoreSlot restores the work slot's KV cache from disk. No-op when
// EngineURL is empty (Ollama/vLLM).
func (r *Runner) restoreSlot(jobID string) error {
	if r.EngineURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"filename": jobID + ".bin"})
	resp, err := http.Post(r.EngineURL+"/slots/"+fmt.Sprint(r.WorkSlotID)+"?action=restore",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return fmt.Errorf("restore slot %d: HTTP %d", r.WorkSlotID, resp.StatusCode)
	}
	return nil
}

// Run claims job id, runs it on driver and records the outcome in the store
// and in jobs.jsonl. A nil error means the agent command exited 0.
func (r *Runner) Run(ctx context.Context, id, round, driver, note string) (Job, error) {
	if driver == "" {
		driver = "local"
	}
	j, err := r.Store.Claim(id, round, driver)
	if err != nil {
		return j, err
	}
	j.Model = ModelFor(driver)
	j.State.Model = j.Model
	j.Status = Running
	j.StartedAt = time.Now().UTC().Format(time.RFC3339)
	r.Store.SetStatus(id, j.State)

	// Restore the work slot's KV cache (no-op when EngineURL is empty).
	if err := r.restoreSlot(id); err != nil {
		return j, fmt.Errorf("restore slot: %w", err)
	}

	var rec *queue.JobRecord
	if r.History != nil {
		rec = r.History.Start(queue.Message{ID: id, Type: queue.KindAgent, Source: "agent-run"}, j.Model, driver == "local", queue.BackgroundQueue)
		rec.Kind, rec.Title, rec.Driver = queue.KindAgent, j.Title, driver
	}

	prompt := r.Prompt(j, driver, note)
	effort := j.ReasoningEffort
	if effort == "" {
		effort = queue.DefaultEffort
	}
	cmdf := r.Command
	if cmdf == nil {
		cmdf = defaultCommand
	}
	cmd := cmdf(driver, prompt, effort)
	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Tee != nil {
		w = io.MultiWriter(&buf, r.Tee)
	}
	cmd.Stdout, cmd.Stderr = w, w

	// Start the command; if SignalFile is set, poll it in a goroutine.
	if err := cmd.Start(); err != nil {
		return j, err
	}
	if r.SignalFile != "" {
		go r.signalWatcher(j.ID, cmd)
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	return r.finish(j, rec, buf.String(), runErr)
}

// signalWatcher polls SignalFile every second. When it appears (written by
// the Discord watcher when the user sends a message), it checks whether the
// engine's chat slot is free. If free, it writes ReplySignalFile and lets
// the round finish (agent-run sends a light reply after completion). If the
// slot is busy, it kills the running command and pauses the job as before.
func (r *Runner) signalWatcher(id string, cmd *exec.Cmd) {
	for {
		if _, err := os.Stat(r.SignalFile); err == nil {
			// Signal file exists: user wants to talk.
			slotBusy := true
			if r.EngineSlotCheck != nil {
				slotBusy = r.EngineSlotCheck()
			}
			if !slotBusy && r.ReplySignalFile != "" {
				// Slot is free: defer reply until the round completes.
				// Write the reply-signal so agent-run knows to answer.
				_ = os.WriteFile(r.ReplySignalFile, nil, 0o644)
				return
			}
			// Slot busy (or no reply path configured): save cache, kill, and pause.
			_ = r.saveSlot(id)  // best-effort; don't fail if engine is down
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			r.Store.Pause(id)
			return
		}
		time.Sleep(time.Second)
	}
}

// Finish records the result of a round that ran outside Run (for example one
// resumed after a reboot). exitCode 0 is done.
func (r *Runner) Finish(id string, exitCode int, output string) (Job, error) {
	j, err := r.Store.Get(id)
	if err != nil {
		return j, err
	}
	var runErr error
	if exitCode != 0 {
		runErr = fmt.Errorf("agent exited with status %d", exitCode)
	}
	return r.finish(j, nil, output, runErr)
}

func (r *Runner) finish(j Job, rec *queue.JobRecord, out string, runErr error) (Job, error) {
	id := j.ID
	r.Store.WriteFile(id, "transcript.txt", []byte(out))
	r.Store.WriteFile(id, "output.txt", []byte(Summary(out)+"\n"))

	pr := ""
	if m := prRe.FindAllString(out, -1); len(m) > 0 {
		pr = m[len(m)-1]
	} else if j.Branch != "" {
		lookup := r.PRLookup
		if lookup == nil {
			lookup = defaultPRLookup
		}
		pr = lookup(j.Branch)
	}
	now := time.Now().UTC()
	j.FinishedAt = now.Format(time.RFC3339)
	if st, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
		j.Duration = now.Sub(st).Round(time.Second).String()
	}
	j.PRURL, j.Summary, j.PID = pr, Summary(out), 0
	// If signalWatcher already paused the job, don't overwrite Paused → Failed.
	cur, _ := r.Store.Get(id)
	if cur.Status == Paused {
		j.Status = Paused
	} else if runErr != nil {
		j.Status, j.Error = Failed, runErr.Error()
	} else {
		j.Status, j.Error = Done, ""
	}
	if err := r.Store.SetStatus(id, j.State); err != nil {
		return j, err
	}
	if r.History != nil {
		if rec == nil {
			rec = r.History.Start(queue.Message{ID: id, Type: queue.KindAgent, Source: "agent-run"}, j.Model, j.RanWith == "local", queue.BackgroundQueue)
			rec.Kind, rec.Title, rec.Driver = queue.KindAgent, j.Title, j.RanWith
			rec.StartedAt = j.StartedAt
		}
		rec.PRURL, rec.Summary = pr, j.Summary
		if runErr != nil {
			r.History.Fail(rec, runErr)
		} else {
			r.History.Complete(rec)
		}
	}
	return j, runErr
}
