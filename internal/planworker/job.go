// Package planworker is the worker-side handler for plan_epic jobs
// (docs/work-board/PLAN.md, Phase B, B2). plane-bridge publishes a
// plan_epic job at the front of the background queue when a plan:<model>
// label lands on an epic (B1); this handler dispatches the job to the
// model named in it — a local planner runs on its engine, swapping the
// model in and restoring the previous one when it is not loaded, and a
// cloud planner runs on its API. The loading rule (PLAN.md, "Model
// registry"): only one big model fits in VRAM, and the small always-on
// front-end model stays up throughout a swap.
package planworker

import (
	"encoding/json"
	"errors"
	"fmt"
)

// TriggerPlan is the trigger value of a plan_epic job's instructions.
const TriggerPlan = "plan_epic"

// Job is a plan_epic job as published by plane-bridge (B1). It is the
// decoded instructions JSON — the contract the bridge documents in
// internal/bridge (instructions()).
type Job struct {
	Trigger string `json:"trigger"`
	Board   Board  `json:"board"`
	Item    Item   `json:"item"`

	// ModelID is the planner from the epic's plan:<model> label.
	ModelID string `json:"model_id"`
	// Effort is the planning effort from an effort:<v> label
	// (low/medium/high, default high in the bridge).
	Effort string `json:"effort"`
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
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SequenceID int      `json:"sequence_id"`
	Parent     string   `json:"parent"`
	State      string   `json:"state"`
	Labels     []string `json:"labels"`
}

// Parse decodes the instructions JSON of a plan_epic job. It rejects
// anything that is not a plan job (a different trigger) or a plan job
// without a model, so a caller can use "it parsed" as "it is a plan job".
func Parse(instructions string) (*Job, error) {
	var j Job
	if err := json.Unmarshal([]byte(instructions), &j); err != nil {
		return nil, fmt.Errorf("parse plan job: %w", err)
	}
	if j.Trigger != TriggerPlan {
		return nil, fmt.Errorf("not a plan_epic job (trigger %q)", j.Trigger)
	}
	if j.ModelID == "" {
		return nil, errors.New("plan job has no model_id")
	}
	return &j, nil
}
