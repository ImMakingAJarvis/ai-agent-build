// Package planworker is the worker-side handler for plan_epic and
// replan_epic jobs (docs/work-board/PLAN.md, Phase B, B2/B5). plane-bridge
// publishes a plan_epic job at the front of the background queue when a
// plan:<model> label lands on an epic (B1); this handler dispatches the
// job to the model named in it — a local planner runs on its engine,
// swapping the model in and restoring the previous one when it is not
// loaded, and a cloud planner runs on its API. The loading rule (PLAN.md,
// "Model registry"): only one big model fits in VRAM, and the small
// always-on front-end model stays up throughout a swap.
package planworker

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Trigger names in a planning job's instructions (the bridge keeps its
// own copies of the same strings — the two sides only talk through the
// board and the queue).
const (
	// TriggerPlan is the trigger value of a plan_epic job's instructions.
	TriggerPlan = "plan_epic"
	// TriggerReplan is the trigger value of a replan_epic job's
	// instructions (B5): the epic already has a delivered plan and was
	// moved back to Planning; the plan runs again with the epic's
	// comments as feedback.
	TriggerReplan = "replan_epic"
)

// Job is a plan_epic or replan_epic job as published by plane-bridge
// (B1/B5). It is the decoded instructions JSON — the contract the bridge
// documents in internal/bridge (instructions()).
type Job struct {
	Trigger string `json:"trigger"`
	Board   Board  `json:"board"`
	Item    Item   `json:"item"`

	// ModelID is the planner from the epic's plan:<model> label.
	ModelID string `json:"model_id"`
	// Effort is the planning effort from an effort:<v> label
	// (low/medium/high, default high in the bridge).
	Effort string `json:"effort"`

	// Comments are the epic's comments as plain text, oldest first
	// (re-plan feedback, B5). The bridge strips the bot's own comments;
	// empty for plan_epic jobs.
	Comments []string `json:"comments,omitempty"`
}

// Board identifies the Plane board the epic lives on.
type Board struct {
	URL       string `json:"url"`
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
	ProjectID string `json:"project_id"`
}

// Item is the epic the plan is for.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SequenceID  int      `json:"sequence_id"`
	Parent      string   `json:"parent"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
}

// Parse decodes the instructions JSON of a planning job. It rejects
// anything that is not a planning job (a different trigger) or a planning
// job without a model, so a caller can use "it parsed" as "it is a
// planning job".
func Parse(instructions string) (*Job, error) {
	var j Job
	if err := json.Unmarshal([]byte(instructions), &j); err != nil {
		return nil, fmt.Errorf("parse plan job: %w", err)
	}
	if j.Trigger != TriggerPlan && j.Trigger != TriggerReplan {
		return nil, fmt.Errorf("not a planning job (trigger %q)", j.Trigger)
	}
	if j.ModelID == "" {
		return nil, errors.New("plan job has no model_id")
	}
	return &j, nil
}
