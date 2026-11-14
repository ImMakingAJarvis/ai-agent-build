package agentjob

import (
	"bytes"
	"context"
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/power"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

var prRe = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// Runner runs claimed jobs and records the result.
type Runner struct {
	Store   *Store
	History *queue.History
	// Command builds the agent command for a driver, full prompt, and reasoning
	// effort level. The default is agent-local (local, and igpu pointed at the
	// iGPU engine) or claude --model $AGENT_STAGE2_MODEL (sonnet) /
	// $AGENT_OPUS_MODEL (opus).
	Command func(driver, prompt, effort string) *exec.Cmd
	// PhotoCommand builds the executor for a photo-tagged job — a photo job
	// runs as plain code, with NO model round (no brain, no iGPU helper).
	// The default is `jarvis-photo make --job <id>`, which is
	// self-sufficient: it starts the art service if it is down, waits for
	// the model to load, generates, delivers the image to Discord, marks the
	// job sent, and queues the job's optional follow-up.
	PhotoCommand func(jobID string) *exec.Cmd
	// PRLookup finds a PR URL for a branch when the output has none. The
	// default asks gh. Return "" if there is none.
	PRLookup func(branch string) string
	// Brief returns the stage-3 rules for a driver. The default reads
	// $JARVIS_CONF/stage3-<driver>.txt.
	Brief func(driver string) string
	// StackStatus returns the machine snapshot included in the round
	// prompt (default: runs jarvis-stack-status). Return "" to omit it.
	StackStatus func() string
	// Capabilities returns the capability catalog included in the round
	// prompt (default: runs `jarvis-mq caps` — the event toolbox: where
	// each capability runs, what it costs, queue depth, typical runtime).
	// Return "" to omit it.
	Capabilities func() string
	// Mailbox returns the waiting mail for the round prompt (plan §3.8,
	// EVB-36 — the mailbox drain step): the driver's agent identity
	// (local → brain, igpu → igpu-helper, sonnet → sonnet, opus →
	// planner, superagent → brain) sees its inbox in its own round.
	// Default: peeks it with `jarvis-mq inbox`. Return "" to omit it
	// (empty inbox or tool missing — mail is durable, nothing is lost).
	Mailbox func(driver string) string
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
	// StopSignalFile, if non-empty, is polled during job execution. When it
	// appears (written by the dashboard's Stop button or `jarvis-jobs stop`),
	// the running command is killed and the job is parked: out of the active
	// queue, so the agent-run loop skips to the next work. The file is
	// consumed, and a stale file from a previous round is cleared by
	// agent-run before it starts the next one.
	StopSignalFile string
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

	// restartSigMu guards rsig: the content of the restart-signal file that
	// interrupted the current round (e.g. "!fixnow"), captured by
	// signalWatcher before it consumes the file. Run() reads it after Wait to
	// decide whether the killed round's context should be saved (fixnow.go).
	restartSigMu sync.Mutex
	rsig         string
}

// setRestartSig records what triggered the round's restart (fixnow.go).
func (r *Runner) setRestartSig(s string) {
	r.restartSigMu.Lock()
	r.rsig = s
	r.restartSigMu.Unlock()
}

// restartSig returns what triggered the round's restart, "" for a fresh run.
func (r *Runner) restartSig() string {
	r.restartSigMu.Lock()
	defer r.restartSigMu.Unlock()
	return r.rsig
}

// readRestartSignal returns the trimmed content of a restart-signal file —
// the command that wrote it ("!fixnow", "!restart", "!photonow") — or "" when
// the file is missing or empty.
func readRestartSignal(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ModelFor names the model a driver uses.
func ModelFor(driver string) string {
	if driver == "sonnet" {
		if m := os.Getenv("AGENT_STAGE2_MODEL"); m != "" {
			return m
		}
		return "sonnet"
	}
	if driver == "opus" {
		if m := os.Getenv("AGENT_OPUS_MODEL"); m != "" {
			return m
		}
		return "opus"
	}
	if driver == "igpu" {
		if m := os.Getenv("IGPU_MODEL"); m != "" {
			return m
		}
		return "jarvis-igpu"
	}
	if driver == "superagent" {
		// The Super Agent's loop runs on the local brain by default;
		// SUPERAGENT_MODEL pins a different engine model.
		if m := os.Getenv("SUPERAGENT_MODEL"); m != "" {
			return m
		}
		return ModelFor("local")
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
// sync with the jarvis-engine-igpu container (docker-compose.yml, service
// engine-igpu).
func igpuContextTokens() string {
	if v := os.Getenv("IGPU_CONTEXT_TOKENS"); v != "" {
		return v
	}
	return "32768"
}

func defaultCommand(driver, prompt, effort string) *exec.Cmd {
	// The Super Agent runs its own plan→act→verify loop (cmd/superagent)
	// instead of the Claude Code harness. The round prompt is its goal, it
	// works in the inherited CWD (the repo), and it streams plain progress
	// to stdout (the Runner tees that to the transcript). Exit 0 = goal met.
	if driver == "superagent" {
		env := append([]string{}, os.Environ()...)
		env = stripEnv(env, "JARVIS_MODEL")
		env = append(env, "JARVIS_MODEL="+ModelFor(driver))
		cmd := exec.Command("superagent", prompt)
		cmd.Env = env
		return cmd
	}
	bin := "agent-local"
	args := []string{"--dangerously-skip-permissions"}
	if driver == "sonnet" || driver == "opus" {
		// The paid API drivers run the claude binary pinned to their model
		// (sonnet, or opus for the harder rounds the local model can't handle).
		bin = "claude"
		args = []string{"--model", ModelFor(driver), "--dangerously-skip-permissions"}
	}
	canon := queue.CanonicalEffort(effort, "coding")
	env := append([]string{}, os.Environ()...)
	// JARVIS_MODEL tells the round which model is driving it, so the agent
	// can prefix its board comments with the model name (P6).
	env = stripEnv(env, "JARVIS_MODEL")
	env = append(env, "JARVIS_MODEL="+ModelFor(driver))
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

// defaultStackStatus runs jarvis-stack-status and returns its report for
// the round prompt. The report is kept even on a non-zero exit (the tool
// prints the full report and exits 1 when something is down — a down
// engine is exactly the state a round needs to see). It returns "" only
// when the tool is missing, times out, or prints no report.
func defaultStackStatus() string {
	bin := "/usr/local/bin/jarvis-stack-status"
	if _, err := os.Stat(bin); err != nil {
		bin = "jarvis-stack-status"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return "" // never block the round on a wedged snapshot
	}
	if err != nil && !strings.Contains(s, "STACK") {
		return "" // missing tool or no report
	}
	return s
}

func (r *Runner) stackStatus() string {
	if r.StackStatus != nil {
		return r.StackStatus()
	}
	return defaultStackStatus()
}

// defaultCapabilities runs `jarvis-mq caps` and returns the catalog for the
// round prompt: one line per capability — where it runs, the cost
// ("needs brain out" / "runs alongside you"), the current queue depth, the
// typical runtime. The lines are designed to be read, not parsed. Like the
// snapshot, a non-zero exit still yields output (the depths print as "-"
// when the broker is down); it returns "" only when the tool is missing or
// prints no catalog, so a wedged toolbox never blocks a round.
func defaultCapabilities() string {
	bin := "/usr/local/bin/jarvis-mq"
	if _, err := os.Stat(bin); err != nil {
		bin = "jarvis-mq"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "caps").CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return "" // never block the round on a wedged catalog
	}
	if err != nil && !strings.Contains(s, "where=") {
		return "" // missing tool or no catalog lines
	}
	return s
}

func (r *Runner) capabilities() string {
	if r.Capabilities != nil {
		return r.Capabilities()
	}
	return defaultCapabilities()
}

// MailboxIdentityFor maps a round driver to the agent identity whose inbox
// its round reads (plan §3.8, EVB-36): a local round IS the brain, an igpu
// round is the iGPU helper, and the API drivers are their own identities.
// planner has no driver of its own yet — its rounds run on the opus driver,
// so opus reads agent.planner.inbox. superagent runs on the local brain.
func MailboxIdentityFor(driver string) string {
	switch driver {
	case "igpu":
		return "igpu-helper"
	case "sonnet":
		return "sonnet"
	case "opus":
		return "planner"
	default:
		return "brain"
	}
}

// defaultMailbox peeks the driver's identity inbox and returns the
// `jarvis-mq inbox` output for the round prompt (plan §3.8, EVB-36). The
// peek is non-destructive: the messages stay in the inbox until the round
// takes them, and a wake keeps its message for the woken round. It returns
// "" on an empty or missing inbox or a wedged tool — mail is durable on the
// broker, so a missed peek is retried next round, never lost.
func defaultMailbox(driver string) string {
	id := MailboxIdentityFor(driver)
	bin := "/usr/local/bin/jarvis-mq"
	if _, err := os.Stat(bin); err != nil {
		bin = "jarvis-mq"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "inbox", id, "-n", "10").CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return "" // never block the round on a wedged broker
	}
	if err != nil || s == "" || s == queue.MailboxQueueName(id)+": empty" {
		return ""
	}
	return s
}

func (r *Runner) mailbox(driver string) string {
	if r.Mailbox != nil {
		return r.Mailbox(driver)
	}
	return defaultMailbox(driver)
}

// mailboxInstructions is the one-liner that tells the round what to do with
// the mail (EVB-36): act on it in this round, and clear it only once it is
// handled.
const mailboxInstructions = "Process this mail in this round: a review_request gets its review done and a reply via `jarvis-mq answer <from> --corr <corr> \"<verdict and findings>\"`; a question gets an `answer` the same way; an fyi/handoff gets read, then cleared with `jarvis-mq take <identity>`."

// mailboxSection renders the round prompt's mailbox block (plan §3.8,
// EVB-36 — the agent-run/mailbox drain step): the driver's identity sees
// its waiting mail at the start of its round. Empty inbox → no section, so
// an idle round is prompt-identical to before.
func (r *Runner) mailboxSection(driver string) string {
	mail := r.mailbox(driver)
	if mail == "" {
		return ""
	}
	id := MailboxIdentityFor(driver)
	var b strings.Builder
	fmt.Fprintf(&b, "\nMAILBOX — waiting mail in %s (the agent-to-agent channel, plan §3.8):\n%s\n", queue.MailboxQueueName(id), mail)
	fmt.Fprintf(&b, "%s\n", mailboxInstructions)
	return b.String()
}

// continuationBlock renders the resume context for a woken round (plan
// §3.5, EVB-16): the job's continuation.md — what it was doing, what is in
// flight, the exact next step, written by the yielding round — plus the
// results that arrived on its reply queue while it waited. Every cross-model
// hop is visible: the woken round is handed what it was doing and what came
// back, clearly marked, before the job's original instructions. It returns
// "" when the job has no continuation.md (it never yielded) — an ordinary
// round is prompt-identical to before.
func (r *Runner) continuationBlock(j Job) string {
	if r.Store == nil {
		return ""
	}
	md, err := os.ReadFile(r.Store.ContinuationMDPath(j.ID))
	if err != nil {
		return ""
	}
	// Correlation ids to report on: the spec's WaitOn (Yield keeps it in
	// step), falling back to the continuation's twin.
	waitOn := j.WaitOn
	if len(waitOn) == 0 {
		if c, cerr := r.Store.ReadContinuation(j.ID); cerr == nil && c != nil {
			waitOn = c.WaitOn
		}
	}
	// Result refs, in priority order: the wake record (the wake that produced
	// this round), the results sidecar (a result that raced the wake, or a
	// re-yield chain), then the continuation's refs.
	refs := map[string]string{}
	rec, werr := r.Store.ReadWake(j.ID)
	for corr, ref := range rec.Results {
		refs[corr] = ref
	}
	if res, rerr := r.Store.RecordedResults(j.ID); rerr == nil {
		for corr, rr := range res {
			if _, ok := refs[corr]; !ok {
				refs[corr] = rr.Ref
			}
		}
	}
	if c, cerr := r.Store.ReadContinuation(j.ID); cerr == nil && c != nil {
		for corr, ref := range c.ResultRefs {
			if _, ok := refs[corr]; !ok {
				refs[corr] = ref
			}
		}
	}

	var b strings.Builder
	b.WriteString("\n=== CONTINUATION — you yielded earlier; this round resumes the job, it does not start it ===\n")
	b.WriteString(strings.TrimSpace(string(md)))
	b.WriteString("\n")
	if len(waitOn) > 0 {
		b.WriteString("\nRESULTS — what came back while the job waited:\n")
		for _, corr := range waitOn {
			if ref, ok := refs[corr]; ok {
				fmt.Fprintf(&b, "- %s: %s\n", corr, ref)
			} else {
				fmt.Fprintf(&b, "- %s: (not arrived yet)\n", corr)
			}
		}
		if werr == nil && rec.Reason == WakeTimeout {
			b.WriteString("\nWARNING: the wait deadline passed before every result arrived — the results above may be PARTIAL. Check which corrs are still missing before acting on them.\n")
		}
		fmt.Fprintf(&b, "Full result messages (payloads, artifacts, errors): jarvis-mq results %s\n", queue.FlowQueueName(j.ID))
	}
	b.WriteString("=== END CONTINUATION ===\n")
	return b.String()
}

// capsCostModel is the cost heuristic in the round prompt (plan §3.7,
// EVB-31): the swap bracket is the expensive part, and one window is
// shared by everything submitted before the yield — so batch the GPU
// requests. The ~2 min figure is measured, not guessed (2026-11-12
// Phase 4 gate run: window open → brain back, image included —
// docs/e2e-event-flow-gate-2026-11-12.md).
const capsCostModel = "Cost model: submitting a capability that is \"needs brain out\" costs ~2 min of swap (the window bracket, shared); batch requests — if you will need several such capabilities, submit them all before yielding (one window, one wake). Capabilities that \"run alongside you\" cost no window; prefer a cpu/igpu placement when it is good enough."

// Prompt is the stage-3 rules, the machine snapshot, the capability
// catalog (the event toolbox), the driver's waiting mailbox mail (the
// mailbox drain step, plan §3.8), and the job text (plus any typed note).
// A woken round additionally gets its continuation (continuationBlock)
// between the branch line and the instructions.
func (r *Runner) Prompt(j Job, driver, note string) string {
	var b strings.Builder
	b.WriteString(r.brief(driver))
	if snap := r.stackStatus(); snap != "" {
		fmt.Fprintf(&b, "\nMACHINE SNAPSHOT at round start (jarvis-stack-status):\n%s\nTreat this as the current state of the machine: containers, loaded engines + VRAM, services, job queue, swap window. Do not re-discover the layout (systemctl list-units, ss -tlnp, docker ps, curl to engine ports). Probe deeper only when the job's evidence contradicts this snapshot, or while debugging a specific failure.\n", snap)
	}
	// The event toolbox (plan §3.7, EVB-31): the brain sees its
	// capabilities and their cost every round, so it reaches for the
	// architecture instead of doing the work itself. Omitted when the
	// catalog is empty (tool missing or the hook returns "").
	if caps := r.capabilities(); caps != "" {
		fmt.Fprintf(&b, "\nCAPABILITIES (jarvis-mq caps — the event toolbox; submit with `jarvis-mq submit <cap>`, the Result lands on $JARVIS_REPLY_QUEUE):\n%s\n%s\n", caps, capsCostModel)
	}
	// The mailbox drain step (plan §3.8, EVB-36): the driver's identity sees
	// its waiting mail at the start of the round — a review request, a
	// question, an fyi. Omitted when the inbox is empty, so an idle round is
	// prompt-identical to before.
	b.WriteString(r.mailboxSection(driver))
	fmt.Fprintf(&b, "\n=== YOUR JOB THIS ROUND: %s — %s ===\n", j.ID, j.Title)
	if j.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s\n", j.Branch)
	}
	if cont := r.continuationBlock(j); cont != "" {
		b.WriteString(cont)
	}
	// A round cut off by a !fixnow preemption resumes with its saved context
	// (fixnow.go). The check always runs and is empty for a novel job; it
	// consumes the record so the block appears exactly once.
	if fc := TakeFixNowContext(j.ID); fc != nil {
		b.WriteString(fixNowResumeBlock(fc))
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

// streamSessionID returns the Claude Code session id of a round whose
// output is a stream-json transcript — the final result event's id,
// falling back to the first system/init id. "" when the transcript
// carries no session id (the command died before init, or it is not a
// stream-json transcript at all).
func streamSessionID(out string) string {
	var initSID string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		sid, _ := ev["session_id"].(string)
		if sid == "" {
			continue
		}
		switch ev["type"] {
		case "system":
			if initSID == "" {
				initSID = sid
			}
		case "result":
			return sid
		}
	}
	return initSID
}

// recordSessionID stores the yielding round's Claude Code session id in
// the job's continuation.json (plan tasks 16/18): a future resume can
// re-attach the round's conversation with --resume <sid>. The RMW takes
// the job lock (the one Claim uses) so it does not race the switchboard's
// own continuation updates; a lost write only costs one result ref, which
// RecordResult re-adds and which results.json holds authoritatively.
func (r *Runner) recordSessionID(id, sid string) {
	lock, err := os.OpenFile(filepath.Join(r.Store.JobDir(id), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	c, err := r.Store.ReadContinuation(id)
	if err != nil || c.SessionID == sid {
		return
	}
	c.SessionID = sid
	_ = r.Store.WriteContinuation(c)
}

// claudeSessionFile is the on-disk transcript of a Claude Code session:
// $HOME/.claude/projects/<cwd, non-alphanumerics → "-">/<sid>.jsonl — the
// same layout the claude binary uses, for the same working directory the
// round ran in. "" when HOME is unknown.
func claudeSessionFile(cwd, sid string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	slug := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, cwd)
	return filepath.Join(home, ".claude", "projects", slug, sid+".jsonl")
}

// resumeSessionID returns the session id a woken job's round should
// re-attach with --resume, or "" for the ordinary fresh round: the
// continuation must record a session id (finish records it when a round
// yields, plan tasks 16/18) and the session file must still be on disk.
// The experiment behind the flag: docs/benchmarks/resume-vs-continuation.md.
func (r *Runner) resumeSessionID(id string) string {
	c, err := r.Store.ReadContinuation(id)
	if err != nil || c.SessionID == "" {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(claudeSessionFile(cwd, c.SessionID)); err != nil {
		return ""
	}
	return c.SessionID
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
// Photo-tagged jobs skip the model round entirely and run through the code
// executor instead (runPhoto).
func (r *Runner) Run(ctx context.Context, id, round, driver, note string) (Job, error) {
	if driver == "" {
		driver = "local"
	}
	// A round only saves its fixnow context if ITS OWN restart signal was a
	// !fixnow (fixnow.go); reset the captured signal so a reused Runner can
	// never carry one from a previous round into this round's save decision.
	r.setRestartSig("")
	// A stop signal left over from a previous round (one that landed after
	// that round's watcher was gone) must not park this round's job: clear
	// it before the claim. A stop pressed during this round lands after the
	// command has started, and the watcher picks it up from there.
	if r.StopSignalFile != "" {
		_ = os.Remove(r.StopSignalFile)
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
	// A photo-tagged job needs no model round at all (not the brain, not the
	// iGPU helper): the executor is self-sufficient code. The driver argument
	// is irrelevant to it — the swap window that pulled the job guarantees
	// the 4090 is free, and the executor stands up the art service itself.
	if j.HasTag(photo.Tag) {
		return r.runPhoto(ctx, j)
	}
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
	// A woken round re-attaches the session that yielded (EVB-18,
	// docs/benchmarks/resume-vs-continuation.md): the measured win is
	// 71-76% fewer fresh input tokens per round at equal correctness.
	// Only Claude Code drivers re-attach (superagent takes a plain goal);
	// a missing session id or session file falls back to the fresh round,
	// which is what every round did before this.
	if driver != "superagent" {
		if sid := r.resumeSessionID(id); sid != "" {
			cmd.Args = append(cmd.Args[:1], append([]string{"--resume", sid}, cmd.Args[1:]...)...)
		}
	}
	// Tag the session so the owner-message hook (scripts/owner-message-hook.sh)
	// knows this is the job's own session and may drain the message inbox;
	// any other claude session in this repo leaves the inbox alone. cmdf may
	// have set its own env (the igpu driver points ENGINE_URL/LOCAL_MODEL at
	// the helper); start from that, falling back to the process env, so the
	// tag never clobbers the driver's env.
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	// Plus the round's reply queue (plan §3.5): results for this job's
	// correlation ids land on flow.<id>, and `jarvis-mq results flow.<id>`
	// is how a woken round reads them in full.
	base = stripEnv(base, "JARVIS_JOB_ID")
	base = stripEnv(base, "JARVIS_REPLY_QUEUE")
	cmd.Env = append(base, "JARVIS_JOB_ID="+id, "JARVIS_REPLY_QUEUE="+queue.FlowQueueName(id))
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
	if r.SignalFile != "" || r.RestartSignalFile != "" || r.StopSignalFile != "" {
		go r.signalWatcher(j.ID, cmd)
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	out := buf.String()
	// A !fixnow preemption cut this round off: save what it was working on
	// (fixnow.go) so the job's re-run picks up where it stopped instead of
	// starting cold. A plain !restart is a deliberate fresh start and saves
	// nothing. Best-effort — a dead Redis only means the re-run starts cold.
	if sig := r.restartSig(); sig == FixNowSignal {
		tail := boundTranscript(out)
		// Durable copy of the cut-off round, kept separate from transcript.txt
		// (which the re-run truncates at the start of its own round) so the
		// resume block can point the model at the interrupted work. Best-effort:
		// a failed write only means the model has the inline tail, not more.
		if err := os.WriteFile(filepath.Join(r.Store.JobDir(id), FixNowPreemptedFile), []byte(tail), 0o644); err != nil {
			log.Printf("agentjob: write preempted transcript for %s: %v", id, err)
		}
		fc := &FixNowContext{
			JobID:      id,
			Title:      j.Title,
			Driver:     driver,
			Model:      j.Model,
			Round:      j.State.Round,
			SavedAt:    time.Now().UTC().Format(time.RFC3339),
			Transcript: tail,
		}
		if err := SaveFixNowContext(fc); err != nil {
			// The round was killed (runErr is set), so a save failure here is a
			// real Redis error worth seeing — it means the re-run starts cold.
			log.Printf("agentjob: save fixnow context for %s: %v", id, err)
		}
	}
	var wh float64
	if stopPower != nil {
		wh = stopPower()
	}
	return r.finish(j, rec, out, runErr, wh)
}

// defaultPhotoCommand is the code executor for a photo job.
func defaultPhotoCommand(jobID string) *exec.Cmd {
	cmd := exec.Command("jarvis-photo", "make", "--job", jobID)
	cmd.Env = os.Environ()
	return cmd
}

// runPhoto runs a photo-tagged job through the code executor: no prompt
// building, no engine slot, no model. The executor's output is the
// transcript (and the Redis stream); its exit code and the delivery marker
// decide the outcome exactly like a model round's (finish() below applies
// the photo marker check — a done run without <id>.sent is a failure).
func (r *Runner) runPhoto(ctx context.Context, j Job) (Job, error) {
	id := j.ID
	j.Model = "jarvis-photo"
	j.State.Model = j.Model
	j.State.RanWith = "photo"
	j.Status = Running
	j.StartedAt = time.Now().UTC().Format(time.RFC3339)
	r.Store.SetStatus(id, j.State)

	// Sample GPU power for the whole run (nil disables it).
	var stopPower func() float64
	if r.Power != nil {
		stopPower = r.Power(r.Store.JobDir(id))
	}

	var rec *queue.JobRecord
	if r.History != nil {
		rec = r.History.Start(queue.Message{ID: id, Type: queue.KindAgent, Source: "agent-run"}, j.Model, false, queue.BackgroundQueue)
		rec.Kind, rec.Title, rec.Driver = queue.KindAgent, j.Title, "photo"
	}

	cmdf := r.PhotoCommand
	if cmdf == nil {
		cmdf = defaultPhotoCommand
	}
	cmd := cmdf(id)
	// Tag the session so any tooling watching the round knows which job is
	// running (mirrors Run's JARVIS_JOB_ID tag).
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	cmd.Env = append(stripEnv(base, "JARVIS_JOB_ID"), "JARVIS_JOB_ID="+id)
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
	// the dashboard can show the executor's progress as it happens.
	if addr := StreamAddr(); addr != "off" && addr != "none" {
		if pub := NewStreamPublisher(addr, id); pub != nil {
			defer pub.Close()
			w = io.MultiWriter(w, pub)
		}
	}
	cmd.Stdout, cmd.Stderr = w, w

	if err := cmd.Start(); err != nil {
		return j, err
	}
	// Stop/restart signals work on photo runs too: the owner can kill a
	// slow generation from the dashboard, and !restart re-runs it. (The
	// pause signal has no slot semantics here, but the watcher treats a
	// photo run like any other.)
	if r.SignalFile != "" || r.RestartSignalFile != "" || r.StopSignalFile != "" {
		go r.signalWatcher(id, cmd)
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

// signalWatcher polls SignalFile, RestartSignalFile and StopSignalFile
// every second. A restart signal (a "!restart" DM) kills the running command
// and re-queues the job for a fresh run. A stop signal (the dashboard's Stop
// button or `jarvis-jobs stop`) kills the command and parks the job, so the
// agent-run loop skips to the next work. A pause signal (an incoming user
// message) checks whether the engine's chat slot is free. If free, it writes
// ReplySignalFile and lets the round finish (agent-run sends a light reply
// after completion). If the slot is busy, it kills the running command and
// pauses the job.
func (r *Runner) signalWatcher(id string, cmd *exec.Cmd) {
	for {
		if r.RestartSignalFile != "" {
			if _, err := os.Stat(r.RestartSignalFile); err == nil {
				// Capture what triggered the restart ("!fixnow", "!restart",
				// "!photonow") before consuming the file, so the killed round
				// can save its context for the re-run when a !fixnow cut it
				// off (Run() reads r.restartSig() after Wait). A plain restart
				// carries no context.
				r.setRestartSig(readRestartSignal(r.RestartSignalFile))
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
		if r.StopSignalFile != "" {
			if _, err := os.Stat(r.StopSignalFile); err == nil {
				// Owner stopped the work: park before kill, like pause —
				// finish() reads the status after Wait returns, so a job
				// that is already Parked keeps it instead of showing up
				// as failed. The file is consumed so a stale stop never
				// hits the next round.
				if err := r.Store.Stop(id); err == nil && cmd.Process != nil {
					cmd.Process.Kill()
				}
				_ = os.Remove(r.StopSignalFile)
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
			_ = r.saveSlot(id) // best-effort; don't fail if engine is down
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
	// If signalWatcher already paused or restarted the job, don't
	// overwrite its status with the kill's error.
	cur, _ := r.Store.Get(id)
	yielded := cur.Status == Waiting
	// A round that yielded leaves its session id in continuation.json so a
	// resume can re-attach it (plan tasks 16/18 — the EVB-18 experiment
	// measures whether a re-attached session is worth it).
	if yielded {
		if sid := streamSessionID(out); sid != "" {
			r.recordSessionID(id, sid)
		}
	}
	now := time.Now().UTC()
	if !yielded {
		j.FinishedAt = now.Format(time.RFC3339)
		if st, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
			j.Duration = now.Sub(st).Round(time.Second).String()
		}
	}
	j.PRURL, j.Summary, j.PID = pr, Summary(streamResultText(out)), 0
	restarted := false
	switch {
	case cur.Status == Paused:
		j.Status = Paused
	case cur.Status == Parked:
		// Stop: the watcher already took the job out of the queue.
		j.Status = Parked
	case cur.Status == Ready:
		// !restart: the watcher already put the job back in the queue.
		j.Status = Ready
		restarted = true
	case yielded:
		// The round yielded (Store.Yield): it ended on purpose and the job
		// stays waiting for its results — not done, not failed, and it has
		// no FinishedAt (it is not finished).
		j.Status = Waiting
	case runErr != nil:
		j.Status, j.Error = Failed, runErr.Error()
	default:
		j.Status, j.Error = Done, ""
	}
	// Photo jobs: the code executor (jarvis-photo) exits 0 only when the
	// image was generated AND delivered — and the marker is what makes that
	// guarantee checkable after the fact. So a photo run that ends done
	// without the delivery marker is a failure, not a done job: the image
	// was never sent, and the job must stay re-queueable instead of being
	// silently lost.
	photoFailed := false
	if j.Status == Done && j.HasTag(photo.Tag) {
		if _, err := os.Stat(photo.SentPath(r.Store.Dir(), id)); err != nil {
			j.Status, j.Error = Failed, "photo round ended but no delivery marker ("+
				filepath.Base(photo.SentPath(r.Store.Dir(), id))+"): the image was not sent — re-queue to retry"
			photoFailed = true
		}
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
		if runErr != nil || photoFailed {
			failErr := runErr
			switch {
			case photoFailed:
				failErr = errors.New(j.Error)
			case restarted:
				failErr = errors.New("restarted by owner")
			case cur.Status == Parked:
				failErr = errors.New("stopped by owner")
			}
			r.History.Fail(rec, failErr)
		} else {
			r.History.Complete(rec)
		}
	}
	// A photo run that ended without its delivery marker is a failure even
	// though the executor exited 0: the marker IS the proof the image was
	// sent, and callers decide the round's outcome from the returned error
	// (the agent-run loop would otherwise notify a done that never happened).
	if photoFailed {
		return j, errors.New(j.Error)
	}
	return j, runErr
}
