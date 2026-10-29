package agentjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/power"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

var prRe = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// Runner runs claimed jobs and records the result.
type Runner struct {
	Store *Store
	History *queue.History
	// Command builds the agent command for a driver, full prompt, and reasoning
	// effort level. The default is agent-local (local, and igpu pointed at the
	// iGPU engine) or claude --model $AGENT_STAGE2_MODEL (sonnet).
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
	// RestartSignalFile, if non-empty, is polled during job execution. When
	// it appears (written by jarvis-chat on a "!restart" DM), the running
	// command is killed and the job is put back to ready, so the agent-run
	// loop starts the same job over in a fresh conversation. The file is
	// consumed, and a stale file from a previous round is cleared by
	// agent-run before it starts the next one.
	RestartSignalFile string
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
	// Pickup is called after a successful Claim (Discord parrot reaction).
	// Defaults to defaultDiscordPickup, which never fails the job.
	Pickup func(Job)
	// Power, if set, starts a power sampler for a job directory and
	// returns a stop function reporting the accumulated watt-hours.
	// The default (nil) disables sampling. See internal/power.
	Power func(jobDir string) func() float64
}

// ModelFor names the model a driver uses.
func ModelFor(driver string) string {
	if driver == "sonnet" {
		if m := os.Getenv("AGENT_STAGE2_MODEL"); m != "" {
			return m
		}
		return "sonnet"
	}
	if driver == "igpu" {
		if m := os.Getenv("IGPU_MODEL"); m != "" {
			return m
		}
		return "jarvis-igpu"
	}
	if m := os.Getenv("LOCAL_MODEL"); m != "" {
		return m
	}
	return "jarvis-brain"
}

// igpuURL is the base URL of the small always-on iGPU engine.
func igpuURL() string {
	if u := os.Getenv("IGPU_URL"); u != "" {
		return u
	}
	return "http://localhost:11435"
}

// igpuContextTokens is the iGPU engine's context size (its --ctx-size),
// which caps the Claude Code context window for an iGPU round. Keep it in
// sync with the jarvis-engine-igpu unit (systemd/jarvis-engine-igpu.service).
func igpuContextTokens() string {
	if v := os.Getenv("IGPU_CONTEXT_TOKENS"); v != "" {
		return v
	}
	return "32768"
}

func defaultCommand(driver, prompt, effort string) *exec.Cmd {
	bin := "agent-local"
	args := []string{"--dangerously-skip-permissions"}
	if driver == "sonnet" {
		bin = "claude"
		args = []string{"--model", ModelFor(driver), "--dangerously-skip-permissions"}
	}
	canon := queue.CanonicalEffort(effort, "coding")
	env := append([]string{}, os.Environ()...)
	if driver == "igpu" {
		// The small iGPU model: agent-local pointed at the iGPU engine.
		// agent-local honours pre-set LOCAL_MODEL/ENGINE_URL/
		// LOCAL_CONTEXT_TOKENS over its local.env, so strip any stale
		// copies from the inherited environment first. Max output is
		// capped well under the iGPU context so one response can never
		// overflow it.
		env = stripEnv(env, "ENGINE_URL")
		env = stripEnv(env, "LOCAL_MODEL")
		env = stripEnv(env, "LOCAL_CONTEXT_TOKENS")
		env = stripEnv(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS")
		env = append(env,
			"ENGINE_URL="+igpuURL(),
			"LOCAL_MODEL="+ModelFor("igpu"),
			"LOCAL_CONTEXT_TOKENS="+igpuContextTokens(),
			"CLAUDE_CODE_MAX_OUTPUT_TOKENS=16384",
		)
	}
	if canon == queue.EffortOff {
		env = append(env, "CLAUDE_CODE_DISABLE_THINKING=1", "MAX_THINKING_TOKENS=0")
	} else if ce := queue.ClaudeEffort(canon); ce != "" {
		args = append(args, "--effort", ce)
		env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+ce)
	}
	// stream-json emits the agent's thoughts, tool calls, and results as
	// JSON lines as they happen (the dashboard live-streams them, and the
	// job log page parses them); --verbose is required with stream-json in
	// print mode, and --include-partial-messages streams token deltas.
	args = append(args, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	args = append(args, "-p", prompt)
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	return cmd
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
	// Messages the owner sent while the agent was idle (queue site or
	// `jarvis-jobs message`). The running session picks up mid-round
	// messages itself via the owner-message hook; this is the round-start
	// path for anything that was waiting.
	if msgs, err := inbox.Drain(); err == nil && strings.TrimSpace(msgs) != "" {
		fmt.Fprintf(&b, "\n\nMessages from me sent while the agent was idle (they take priority):\n%s\n", msgs)
	}
	return b.String()
}

// streamResultText returns the agent's final answer when the output is
// stream-json (a trailing {"type":"result","result":"..."} line), so
// summaries show the reply text instead of the raw event. Any other
// output (plain-text runs, error tails) is returned unchanged.
func streamResultText(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			break
		}
		if ev["type"] == "result" {
			if r, _ := ev["result"].(string); strings.TrimSpace(r) != "" {
				return r
			}
		}
		break
	}
	return out
}

// stripEnv returns env without any entries for key, so appending a fresh
// value below can never leave a duplicate behind.
func stripEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
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
	pickup := r.Pickup
	if pickup == nil {
		pickup = defaultDiscordPickup
	}
	pickup(j)
	j.Model = ModelFor(driver)
	j.State.Model = j.Model
	j.Status = Running
	j.StartedAt = time.Now().UTC().Format(time.RFC3339)
	r.Store.SetStatus(id, j.State)

	// Sample GPU power for the whole run (nil disables it).
	var stopPower func() float64
	if r.Power != nil {
		stopPower = r.Power(r.Store.JobDir(id))
	}

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
	effort := queue.CanonicalEffort(j.ReasoningEffort, "coding")
	cmdf := r.Command
	if cmdf == nil {
		cmdf = defaultCommand
	}
	cmd := cmdf(driver, prompt, effort)
	// Tag the session so the owner-message hook (scripts/owner-message-hook.sh)
	// knows this is the job's own session and may drain the message inbox;
	// any other claude session in this repo leaves the inbox alone.
	cmd.Env = append(stripEnv(os.Environ(), "JARVIS_JOB_ID"), "JARVIS_JOB_ID="+id)
	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Tee != nil {
		w = io.MultiWriter(&buf, r.Tee)
	}
	// Stream the same bytes into transcript.txt so a running job has a log.
	if tf, err := os.OpenFile(filepath.Join(r.Store.JobDir(id), "transcript.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		defer tf.Close()
		w = io.MultiWriter(w, tf)
	}
	// Live stream: publish each output line to the job's Redis stream so
	// the dashboard can show the worker's thoughts and progress as they
	// happen. The transcript file above stays the durable copy; a missing
	// or dead Redis never affects the job itself.
	if addr := StreamAddr(); addr != "off" && addr != "none" {
		if pub := NewStreamPublisher(addr, id); pub != nil {
			defer pub.Close()
			w = io.MultiWriter(w, pub)
		}
	}
	cmd.Stdout, cmd.Stderr = w, w

	// Start the command; if a signal file is set, poll it in a goroutine.
	if err := cmd.Start(); err != nil {
		return j, err
	}
	if r.SignalFile != "" || r.RestartSignalFile != "" {
		go r.signalWatcher(j.ID, cmd)
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	var wh float64
	if stopPower != nil {
		wh = stopPower()
	}
	return r.finish(j, rec, buf.String(), runErr, wh)
}

// signalWatcher polls SignalFile and RestartSignalFile every second. A
// restart signal (a "!restart" DM) kills the running command and re-queues
// the job for a fresh run. A pause signal (an incoming user message) checks
// whether the engine's chat slot is free. If free, it writes ReplySignalFile
// and lets the round finish (agent-run sends a light reply after completion).
// If the slot is busy, it kills the running command and pauses the job.
func (r *Runner) signalWatcher(id string, cmd *exec.Cmd) {
	for {
		if r.RestartSignalFile != "" {
			if _, err := os.Stat(r.RestartSignalFile); err == nil {
				// Kill first, then re-queue: if the command already exited,
				// Restart() is a no-op on a non-running job, so a late
				// signal can never re-run a finished job.
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				_ = r.Store.Restart(id)
				_ = os.Remove(r.RestartSignalFile) // consume; don't kill the next round
				return
			}
		}
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
			// Slot busy (or no reply path configured): save cache, pause,
			// then kill. The pause must land in the store before the kill:
			// finish() reads the status after Wait returns, and a job that
			// is already Failed would refuse the Pause and show up as
			// failed instead of paused.
			_ = r.saveSlot(id)  // best-effort; don't fail if engine is down
			if err := r.Store.Pause(id); err == nil && cmd.Process != nil {
				cmd.Process.Kill()
			}
			return
		}
		time.Sleep(time.Second)
	}
}

// Finish records the result of a round that ran outside Run (for example one
// resumed after a reboot). exitCode 0 is done. No sampler ran in this
// process, so the energy comes from whatever the previous sampler persisted
// (power.json) before the reboot — at most one sampling interval is lost.
func (r *Runner) Finish(id string, exitCode int, output string) (Job, error) {
	j, err := r.Store.Get(id)
	if err != nil {
		return j, err
	}
	var runErr error
	if exitCode != 0 {
		runErr = fmt.Errorf("agent exited with status %d", exitCode)
	}
	wh, _ := power.LoadPersisted(r.Store.JobDir(id))
	return r.finish(j, nil, output, runErr, wh)
}

func (r *Runner) finish(j Job, rec *queue.JobRecord, out string, runErr error, wh float64) (Job, error) {
	id := j.ID
	r.Store.WriteFile(id, "transcript.txt", []byte(out))
	// With stream-json the tail of out is a result event; keep summaries
	// human-readable by showing the agent's final answer instead.
	r.Store.WriteFile(id, "output.txt", []byte(Summary(streamResultText(out))+"\n"))

	// Local job cost: sampled energy at the configured $/kWh rate.
	if kwh := power.Kwh(wh); kwh > 0 {
		j.Kwh, j.PowerCost = kwh, power.Cost(wh, power.Rate())
	}

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
	j.PRURL, j.Summary, j.PID = pr, Summary(streamResultText(out)), 0
	// If signalWatcher already paused or restarted the job, don't
	// overwrite its status with the kill's error.
	cur, _ := r.Store.Get(id)
	restarted := false
	switch {
	case cur.Status == Paused:
		j.Status = Paused
	case cur.Status == Ready:
		// !restart: the watcher already put the job back in the queue.
		j.Status = Ready
		restarted = true
	case runErr != nil:
		j.Status, j.Error = Failed, runErr.Error()
	default:
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
		rec.Kwh, rec.PowerCost = j.Kwh, j.PowerCost
		if runErr != nil {
			failErr := runErr
			if restarted {
				failErr = errors.New("restarted by owner")
			}
			r.History.Fail(rec, failErr)
		} else {
			r.History.Complete(rec)
		}
	}
	return j, runErr
}
