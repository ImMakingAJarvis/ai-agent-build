// Package kanban provides a file-based Kanban board for tracking feature development.
//
// It manages epics, tasks, and subtasks with a simple state machine:
//
//	idea -> in-progress -> review -> done
//
// Data persists to ~/.local/share/jarvis/kanban/ as JSON files.
// When a task is marked as ready for execution, it can be enqueued to RabbitMQ
// as an agent job for the worker to pick up.
package kanban

import (
	"time"
)

// Status represents a task's position on the board.
type Status string

const (
	StatusBacklog    Status = "backlog"
	StatusInProgress Status = "in-progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
)

// Epic is a large body of work grouped under a shared theme.
type Epic struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	TaskIDs     []string  `json:"task_ids"`
	CreatedAt   time.Time `json:"created_at"`
}

// Task is a discrete unit of work belonging to an epic.
type Task struct {
	ID        string    `json:"id"`
	EpicID    string    `json:"epic_id"`
	Title     string    `json:"title"`
	Content   string    `json:"content,omitempty"` // detailed instructions / acceptance criteria
	Status    Status    `json:"status"`
	Priority  int       `json:"priority"`         // 1-10, higher = more urgent; 0 treated as 5
	Branch    string    `json:"branch,omitempty"` // git branch created for this task
	Model     string    `json:"model,omitempty"`  // model the task is assigned to
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SubTask is a small checklist item within a task.
type SubTask struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Title     string    `json:"title"`
	Completed bool      `json:"completed"`
	CreatedAt time.Time `json:"created_at"`
}
