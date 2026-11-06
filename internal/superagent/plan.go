package superagent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is the output of the planning phase: the ordered steps the agent
// will execute and the criterion that decides the goal is done.
type Plan struct {
	Steps        []string `json:"steps"`
	DoneCriteria string   `json:"done_criteria"`
}

// PlanPrompt is the system prompt for the planning call.
const PlanPrompt = `You are the planner of an autonomous agent. Break the goal below into a short list of concrete steps.

Rules:
- 1 to 8 steps. Each step is one line of imperative text, specific enough
  that an executor with no other context can carry it out (name files and
  commands where known).
- The steps must cover verification: the final step(s) check that the
  goal's done criteria actually hold.
- "done_criteria" is one or two sentences: how to verify the whole goal is
  achieved (a command, a file to check, an observable fact).

Reply with ONLY a JSON object — no markdown, no code fences, no text before
or after:
{"steps": ["step 1", "step 2"], "done_criteria": "..."}`

// ParsePlan extracts a Plan from a model reply. It tolerates a JSON object
// wrapped in code fences or surrounded by stray text: the first '{' to the
// matching '}' is parsed.
func ParsePlan(reply string) (Plan, error) {
	s := strings.TrimSpace(reply)
	start := strings.Index(s, "{")
	if start < 0 {
		return Plan{}, fmt.Errorf("no JSON object in plan reply")
	}
	end := strings.LastIndex(s, "}")
	if end <= start {
		return Plan{}, fmt.Errorf("unterminated JSON object in plan reply")
	}
	var p Plan
	if err := json.Unmarshal([]byte(s[start:end+1]), &p); err != nil {
		return Plan{}, fmt.Errorf("parse plan JSON: %w", err)
	}
	for i := range p.Steps {
		p.Steps[i] = strings.TrimSpace(p.Steps[i])
	}
	p.Steps = dropEmpty(p.Steps)
	if len(p.Steps) == 0 {
		return Plan{}, fmt.Errorf("plan has no steps")
	}
	if len(p.Steps) > 8 {
		return Plan{}, fmt.Errorf("plan has %d steps (max 8)", len(p.Steps))
	}
	p.DoneCriteria = strings.TrimSpace(p.DoneCriteria)
	if p.DoneCriteria == "" {
		p.DoneCriteria = "all steps completed"
	}
	return p, nil
}

func dropEmpty(steps []string) []string {
	out := steps[:0]
	for _, s := range steps {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// String renders a plan for prompts and logs.
func (p Plan) String() string {
	var b strings.Builder
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	b.WriteString("Done when: " + p.DoneCriteria)
	return b.String()
}
