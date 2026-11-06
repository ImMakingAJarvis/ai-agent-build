package superagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Agent is one Super Agent run: plan the goal, execute the steps with
// tools, verify, report.
type Agent struct {
	Model Model
	Tools *Tools
	Log   io.Writer // step log; nil disables logging

	MaxSteps   int           // step executions across all plans (default 20)
	MaxTurns   int           // model calls per step (default 8)
	Replans    int           // replans allowed (default 2)
	RunTimeout time.Duration // whole-run deadline (default 30m)
}

// New returns an Agent with the defaults above.
func New(model Model, tools *Tools) *Agent {
	return &Agent{
		Model: model, Tools: tools,
		MaxSteps: 20, MaxTurns: 8, Replans: 2, RunTimeout: 30 * time.Minute,
	}
}

// Result is the outcome of a run.
type Result struct {
	OK        bool
	Summary   string
	Plan      Plan
	StepNotes []string // one per completed step: how it was verified
	Turns     int      // total model calls
	Steps     int      // steps executed
	Duration  time.Duration
}

// Run executes the goal and returns the result. A nil error means the
// agent completed its run (the goal may still have failed: see OK).
func (a *Agent) Run(ctx context.Context, goal string) (*Result, error) {
	if a.MaxSteps <= 0 {
		a.MaxSteps = 20
	}
	if a.MaxTurns <= 0 {
		a.MaxTurns = 8
	}
	if a.Replans < 0 {
		a.Replans = 2
	}
	if a.RunTimeout <= 0 {
		a.RunTimeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, a.RunTimeout)
	defer cancel()

	start := time.Now()
	res := &Result{Plan: Plan{}}
	a.log("goal: %s", strings.TrimSpace(goal))

	plan, err := a.plan(ctx, goal, nil, "")
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	res.Plan = plan
	a.log("plan (%d steps): %s", len(plan.Steps), strings.ReplaceAll(plan.String(), "\n", " | "))

	stepsLeft := a.MaxSteps
	replansLeft := a.Replans
	for i := 0; i < len(plan.Steps); i++ {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if stepsLeft <= 0 {
			res.Summary = fmt.Sprintf("step budget of %d exhausted after %d steps", a.MaxSteps, res.Steps)
			res.Duration = time.Since(start)
			a.log("result: FAILED — %s", res.Summary)
			return res, nil
		}
		step := plan.Steps[i]
		a.log("step %d/%d: %s", i+1, len(plan.Steps), step)
		res.Steps++
		stepsLeft--
		outcome, turns, err := a.runStep(ctx, goal, plan, i, step, res.StepNotes)
		res.Turns += turns
		if err != nil {
			return res, err
		}
		switch outcome.kind {
		case kindFinish:
			res.OK = outcome.ok
			res.Summary = outcome.note
			res.Duration = time.Since(start)
			a.log("result: %s — %s", resultWord(res.OK), res.Summary)
			return res, nil
		case kindStepDone:
			res.StepNotes = append(res.StepNotes, outcome.note)
			a.log("  step done: %s", outcome.note)
		case kindReplan:
			if replansLeft <= 0 {
				a.log("  replan requested but budget exhausted: %s (continuing)", outcome.note)
				continue
			}
			replansLeft--
			a.log("  replan: %s", outcome.note)
			np, err := a.plan(ctx, goal, res.StepNotes, outcome.note)
			if err != nil {
				return res, fmt.Errorf("replan: %w", err)
			}
			plan = np
			res.Plan = np
			i = -1 // restart at the top of the new plan
			a.log("new plan (%d steps): %s", len(plan.Steps), strings.ReplaceAll(plan.String(), "\n", " | "))
			continue
		case kindStepFailed:
			a.log("  step failed: %s", outcome.note)
			res.Summary = outcome.note
		}
	}

	// The plan ran out without an explicit finish: verify the goal.
	ok, summary, turns, err := a.verify(ctx, goal, plan, res.StepNotes)
	res.Turns += turns
	if err != nil {
		return res, err
	}
	res.OK = ok
	res.Summary = summary
	res.Duration = time.Since(start)
	a.log("result: %s — %s", resultWord(res.OK), res.Summary)
	return res, nil
}

type outcomeKind int

const (
	kindStepDone outcomeKind = iota
	kindStepFailed
	kindReplan
	kindFinish
)

type outcome struct {
	kind outcomeKind
	note string
	ok   bool
}

// plan asks the model for a plan (or a re-plan, when doneNotes/reason are
// non-empty) and parses it. One nudge is spent on an unparseable reply.
func (a *Agent) plan(ctx context.Context, goal string, doneNotes []string, reason string) (Plan, error) {
	var user strings.Builder
	user.WriteString("Goal:\n" + goal)
	if len(doneNotes) > 0 {
		user.WriteString("\n\nSteps already completed (do not repeat them):\n")
		for i, n := range doneNotes {
			fmt.Fprintf(&user, "%d. %s\n", i+1, n)
		}
	}
	if reason != "" {
		user.WriteString("\n\nWhy we are re-planning: " + reason)
	}
	messages := []Message{
		{Role: "system", Content: PlanPrompt},
		{Role: "user", Content: user.String()},
	}
	reply, err := a.Model.Chat(ctx, messages)
	if err != nil {
		return Plan{}, err
	}
	if p, err := ParsePlan(reply); err == nil {
		return p, nil
	}
	firstErr := err
	messages = append(messages,
		Message{Role: "assistant", Content: reply},
		Message{Role: "user", Content: "That was not valid plan JSON. Reply again with ONLY the JSON object."},
	)
	reply, err = a.Model.Chat(ctx, messages)
	if err != nil {
		return Plan{}, err
	}
	if p, err := ParsePlan(reply); err == nil {
		return p, nil
	}
	return Plan{}, firstErr
}

// executorPrompt is the system prompt for one step.
func (a *Agent) executorPrompt(goal string, plan Plan, doneNotes []string) string {
	var b strings.Builder
	b.WriteString("You are an autonomous executor agent. Work in the directory: " + a.Tools.Guard.WorkDir + "\n\n")
	b.WriteString("Goal:\n" + goal + "\n\nPlan:\n" + plan.String() + "\n\n")
	if len(doneNotes) > 0 {
		b.WriteString("Steps already completed (verified):\n")
		for i, n := range doneNotes {
			fmt.Fprintf(&b, "%d. %s\n", i+1, n)
		}
		b.WriteString("\n")
	}
	b.WriteString("Tools:\n")
	for _, s := range Specs() {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	b.WriteString(`
Rules:
- Every reply MUST end with exactly one ACTION block, nothing after it:
  <ACTION>
  {"tool": "shell", "args": {"cmd": "..."}}
  </ACTION>
- Think briefly in plain text before the block, then act. One action per reply.
- Verify before calling step_done: run the check, read the file, see the
  output. The note must say how the step was verified.
- If you fail the same action twice, call replan (or finish with ok=false
  when the goal is impossible).
- Files and commands are confined to the work dir; blocked actions come
  back as "BLOCKED: ..." observations — do a different thing, never retry
  the blocked command.
- When the whole goal is achieved, call finish with ok=true and a summary
  of what was done. When it cannot be done, call finish with ok=false.`)
	return b.String()
}

// runStep executes one plan step: a bounded loop of model turns until the
// model calls step_done / replan / finish or the turn budget is spent.
func (a *Agent) runStep(ctx context.Context, goal string, plan Plan, i int, step string, doneNotes []string) (outcome, int, error) {
	messages := []Message{
		{Role: "system", Content: a.executorPrompt(goal, plan, doneNotes)},
		{Role: "user", Content: fmt.Sprintf("Execute step %d: %s\n\nStart with your first action.", i+1, step)},
	}
	for turns := 0; turns < a.MaxTurns; {
		if ctx.Err() != nil {
			return outcome{kind: kindStepFailed, note: ctx.Err().Error()}, turns + 1, ctx.Err()
		}
		reply, err := a.Model.Chat(ctx, messages)
		turns++
		if err != nil {
			return outcome{kind: kindStepFailed, note: "model error: " + err.Error()}, turns, nil
		}
		messages = append(messages, Message{Role: "assistant", Content: reply})
		act, err := ParseAction(reply)
		if err != nil {
			a.log("  turn %d: no ACTION block (%v) — nudging", turns, err)
			messages = append(messages, Message{Role: "user", Content: "Your last reply had no valid ACTION block. Reply again, ending with exactly one <ACTION>...</ACTION> block."})
			continue
		}
		switch act.Tool {
		case "step_done":
			return outcome{kind: kindStepDone, note: argStr(act.Args, "note", "done")}, turns, nil
		case "replan":
			return outcome{kind: kindReplan, note: argStr(act.Args, "reason", "replan")}, turns, nil
		case "finish":
			ok := false
			var m map[string]any
			if err := json.Unmarshal(act.Args, &m); err == nil {
				ok, _ = m["ok"].(bool)
			}
			return outcome{kind: kindFinish, note: argStr(act.Args, "summary", ""), ok: ok}, turns, nil
		}
		obs, err := a.Tools.Run(ctx, act)
		if err != nil {
			obs = "ERROR: " + err.Error()
		}
		a.log("  turn %d: %s %s -> %s", turns, act.Tool, shortArg(act), firstLine(obs))
		messages = append(messages, Message{Role: "user", Content: "OBSERVATION (" + act.Tool + "):\n" + obs})
	}
	return outcome{kind: kindStepFailed, note: fmt.Sprintf("no step_done within %d turns", a.MaxTurns)}, a.MaxTurns, nil
}

// verify asks the model whether the goal is actually done, given the step
// notes. Returns ok, summary, turns, error.
func (a *Agent) verify(ctx context.Context, goal string, plan Plan, notes []string) (bool, string, int, error) {
	var user strings.Builder
	user.WriteString("Goal:\n" + goal + "\n\nDone criteria: " + plan.DoneCriteria + "\n\nWhat the executor verified, step by step:\n")
	if len(notes) == 0 {
		user.WriteString("(no completed steps)")
	}
	for i, n := range notes {
		fmt.Fprintf(&user, "%d. %s\n", i+1, n)
	}
	sys := `You are the verifier of an autonomous agent. Decide whether the goal is actually achieved, from the evidence below. Be strict: if the evidence does not show the done criteria holding, say no.

Reply with ONLY a JSON object — no markdown, no code fences:
{"ok": true, "summary": "one or two sentences"}`
	var lastErr error
	for turns := 0; turns < 2; turns++ {
		reply, err := a.Model.Chat(ctx, []Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user.String()},
		})
		if err != nil {
			return false, "", turns + 1, err
		}
		var v struct {
			OK      bool   `json:"ok"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(extractJSON(reply)), &v); err != nil {
			lastErr = err
			continue
		}
		if strings.TrimSpace(v.Summary) == "" {
			v.Summary = "goal " + resultWord(v.OK) + " per verifier"
		}
		return v.OK, strings.TrimSpace(v.Summary), turns + 1, nil
	}
	return false, "verifier did not return valid JSON (" + lastErr.Error() + ")", 2, nil
}

// extractJSON returns the first {...} span of s.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

func (a *Agent) log(format string, args ...any) {
	if a.Log == nil {
		return
	}
	fmt.Fprintf(a.Log, "[superagent] "+format+"\n", args...)
}

func resultWord(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAILED"
}

// firstLine returns the first line of s, capped at 120 chars.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return strings.TrimSpace(s)
}

// shortArg renders the salient argument of an action for the log.
func shortArg(a Action) string {
	switch a.Tool {
	case "shell":
		return firstLine(argStr(a.Args, "cmd", ""))
	case "read_file", "write_file", "list_dir":
		return argStr(a.Args, "path", "")
	case "memory_search":
		return argStr(a.Args, "query", "")
	case "web_fetch":
		return argStr(a.Args, "url", "")
	case "step_done", "replan":
		return firstLine(argStr(a.Args, "note", argStr(a.Args, "reason", "")))
	case "finish":
		return firstLine(argStr(a.Args, "summary", ""))
	default:
		return ""
	}
}
