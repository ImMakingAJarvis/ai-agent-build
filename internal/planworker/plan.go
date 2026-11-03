package planworker

import "context"

// Planner turns one plan job into a plan document and a validated task list
// using the model it is handed. The B3 Pipeline (pipeline.go) implements it
// as the Plan/Decompose/Review flow over the prompts/ files and the task
// JSON schema (docs/work-board/PLAN.md, "Planning pipeline").
type Planner interface {
	// Plan produces the plan document and task list for job using model.
	Plan(ctx context.Context, model Model, job *Job) (*PlanOutput, error)
}
