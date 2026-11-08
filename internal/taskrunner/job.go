// Package taskrunner is the C1 execution loop (docs/work-board/PLAN.md,
// "Execution loop"): it takes a run_task job, reads the card from the
// board, waits for its dependencies, places the executor model, and drives
// a harness (Aider) against the card's files until the acceptance check
// passes — or the card is Blocked.
//
// The loop runs host-side, not in the worker container: the worker mounts
// the repo read-only and has no git, no Python, no Go toolchain. The host
// task-runner (cmd/task-runner) consumes the location queues and calls
// Runner.Run.
package taskrunner

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
)

// TriggerRun is the job trigger this package consumes.
const TriggerRun = "run_task"

// Job is the run_task payload the bridge publishes (the contract in the
// bridge's instructions()): the item's identity plus the board
// coordinates. Item.Description is plain text (stripped HTML); the card's
// real content comes from the board (GetWorkItem + ParseTaskCard) — the
// board is the source of truth, the payload is a pointer.
type Job struct {
	Trigger string `json:"trigger"`
	Board   Board  `json:"board"`
	Item    Item   `json:"item"`
}

// Board names the Plane workspace/project the item lives in.
type Board struct {
	URL       string `json:"url"`
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
	ProjectID string `json:"project_id"`
}

// Item is the card's identity on the board.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SequenceID  int      `json:"sequence_id"`
	Parent      string   `json:"parent"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
}

// Parse decodes and validates a run_task payload. Any other trigger is
// rejected: the task-runner consumes execution jobs only.
func Parse(raw []byte) (*Job, error) {
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("run_task payload: %w", err)
	}
	if j.Trigger != TriggerRun {
		return nil, fmt.Errorf("run_task payload: trigger %q is not %s", j.Trigger, TriggerRun)
	}
	if j.Board.ProjectID == "" {
		return nil, fmt.Errorf("run_task payload: no board project id")
	}
	if j.Item.ID == "" {
		return nil, fmt.Errorf("run_task payload: no item id")
	}
	if j.Item.Parent == "" {
		return nil, fmt.Errorf("run_task payload: %s job for an item without a parent epic", TriggerRun)
	}
	return &j, nil
}

// ModelID is the executor model's registry id from the card's
// model:<id> label, or "" if the card carries none.
func (j *Job) ModelID() string { return labelValue(j.Item.Labels, placement.ModelPrefix) }

// RunOverride is the location pinned by a run:<loc> label, or "" for none.
func (j *Job) RunOverride() string { return labelValue(j.Item.Labels, placement.RunPrefix) }

func labelValue(labels []string, prefix string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}
