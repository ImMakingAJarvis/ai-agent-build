package planworker

import (
	"context"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// Planner turns one plan job into a plan document using the model it is
// handed. B2 ships the single-call BasicPlanner as the default; B3
// replaces it with the Plan/Decompose/Review pipeline (prompts/ files and
// the task JSON schema).
type Planner interface {
	// Plan produces the plan document for job using model.
	Plan(ctx context.Context, model Model, job *Job) (string, error)
}

// BasicPlanner is the B2 default planner: one completion, the epic's
// title in, a plan document out. Its prompt is a placeholder — B3's
// pipeline carries the real planning prompts (epic description and repo
// summary included, per PLAN.md "Planning pipeline").
type BasicPlanner struct{}

func (BasicPlanner) Plan(ctx context.Context, model Model, job *Job) (string, error) {
	prompt := strings.Join([]string{
		"Plan the work in the epic below as a markdown document: goal, " +
			"architecture, files likely touched, risks, and open questions.",
		"",
		"Epic: " + job.Item.Name,
	}, "\n")
	out, err := model.Chat(ctx, []models.ChatMessage{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return "", fmt.Errorf("plan %s: %w", job.Item.Name, err)
	}
	return out, nil
}
