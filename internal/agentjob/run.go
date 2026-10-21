package agentjob

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	Store   *Store
	History *queue.History
	// Command builds the agent command for a driver and full prompt. The
	// default is agent-local (local) or claude --model $AGENT_STAGE2_MODEL.
	Command func(driver, prompt string) *exec.Cmd
	// PRLookup finds a PR URL for a branch when the output has none. The
	// default asks gh. Return "" if there is none.
	PRLookup func(branch string) string
	// Brief returns the stage-3 rules for a driver. The default reads
	// $JARVIS_CONF/stage3-<driver>.txt.
	Brief func(driver string) string
	// Tee, if set, also receives the agent's output live (agent.log).
	Tee io.Writer
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

func defaultCommand(driver, prompt string) *exec.Cmd {
	if driver == "sonnet" {
		return exec.Command("claude", "--model", ModelFor(driver), "--dangerously-skip-permissions", "-p", prompt)
	}
	return exec.Command("agent-local", "--dangerously-skip-permissions", "-p", prompt)
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

	var rec *queue.JobRecord
	if r.History != nil {
		rec = r.History.Start(queue.Message{ID: id, Type: queue.KindAgent, Source: "agent-run"}, j.Model, driver == "local", queue.BackgroundQueue)
		rec.Kind, rec.Title, rec.Driver = queue.KindAgent, j.Title, driver
	}

	prompt := r.Prompt(j, driver, note)
	cmdf := r.Command
	if cmdf == nil {
		cmdf = defaultCommand
	}
	cmd := cmdf(driver, prompt)
	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Tee != nil {
		w = io.MultiWriter(&buf, r.Tee)
	}
	cmd.Stdout, cmd.Stderr = w, w
	runErr := cmd.Run()
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	return r.finish(j, rec, buf.String(), runErr)
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
	if runErr != nil {
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
