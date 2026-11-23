package planworker

import (
	"fmt"
	"regexp"
	"strings"
)

// MinDescriptionChars is the shortest description a model-executed task
// may carry. A task card is the executor's whole brief (docs/work-board/
// PLAN.md: "a fresh context ... without any other context"), so it has to
// spell out the goal, the approach and the files, which does not fit in
// a sentence. A capability task's description is the capability's input
// ("an image of a red bicycle"), so it only has to be non-trivial.
const (
	MinDescriptionChars           = 200
	MinCapabilityDescriptionChars = 15
)

// pointerOnly matches a description that sends the executor elsewhere for
// the work ("Job 2a of docs/plan.md.", "See the plan, section 3.",
// "Implement as described in PLAN.md") instead of carrying it. It is only
// applied to short descriptions: a long one that also cites the plan is
// fine, because the work is spelled out as well.
var pointerOnly = regexp.MustCompile(`(?i)` +
	`\b(see|refer to|per|as (described|specified|outlined|written) in|(described|detailed|specified) in)\b` +
	`[^.]{0,40}\b(plan|spec|document|section|job)\b` +
	`|\b(job|section|step|part)\s+[\w.]+\s+(of|in)\s+(the\s+)?[\w./-]*(plan|\.md)\b`)

// pointerCheckBelow is the length under which a description that matches
// pointerOnly is treated as a bare reference.
const pointerCheckBelow = 400

// CheckDescriptions rejects a task list whose task descriptions do not
// stand on their own: too short to brief an executor, or a pointer at the
// plan document instead of the work. The returned error names every bad
// task, so the decompose retry can tell the model exactly what to fix.
func CheckDescriptions(tl TaskList) error {
	var bad []string
	for _, t := range tl.Tasks {
		if reason := descriptionProblem(t); reason != "" {
			bad = append(bad, fmt.Sprintf("task %q: %s", t.ID, reason))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("task descriptions must carry the work itself, not point at the plan:\n%s",
		strings.Join(bad, "\n"))
}

func descriptionProblem(t Task) string {
	d := strings.TrimSpace(t.Description)
	n := len([]rune(d))
	if t.Capability != "" {
		if n < MinCapabilityDescriptionChars {
			return fmt.Sprintf("description is %d characters; give the capability its full input", n)
		}
		return ""
	}
	if n < MinDescriptionChars {
		return fmt.Sprintf("description is %d characters, need at least %d: write the goal, the approach, "+
			"the functions/types/flags to add and the files to change, copied from the plan into this task", n, MinDescriptionChars)
	}
	if n < pointerCheckBelow && pointerOnly.MatchString(d) {
		return "description refers to the plan or another document instead of saying what to build; " +
			"copy the relevant plan text into the description"
	}
	return ""
}
