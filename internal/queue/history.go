// Package queue provides job history recording: start, finish, model, duration.
package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JobRecord is persisted to disk for every job the worker handles.
type JobRecord struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Source    string `json:"source"`
	Model     string `json:"model"`
	UseLocal bool   `json:"use_local"`
	Queue     string `json:"queue"`
	StartedAt string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Duration  string `json:"duration,omitempty"`
	Status    string `json:"status"` // running, completed, failed, denied
	Error     string `json:"error,omitempty"`
	// Cost fields (populated for API-dispatched jobs).
	EstimatedCost float64 `json:"estimated_cost,omitempty"`   // $ estimated before dispatch
	ActualCost    float64 `json:"actual_cost,omitempty"`      // $ billed after dispatch
	InputTokens   int     `json:"input_tokens,omitempty"`     // estimated input tokens
	OutputTokens  int     `json:"output_tokens,omitempty"`    // estimated output tokens
}

// History records job lifecycle events to a JSONL file.
type History struct {
	mu   sync.Mutex
	file *os.File
	dir  string
}

// NewHistory opens (or creates) a JSONL file for job records.
// The file is written to ~/.local/share/jarvis/jobs.jsonl by default.
func NewHistory() (*History, error) {
	dir := os.Getenv("JARVIS_DATA_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/home/agent"
		}
		dir = filepath.Join(home, ".local", "share", "jarvis")
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "jobs.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open jobs log %s: %w", path, err)
	}

	return &History{file: f, dir: dir}, nil
}

// Close flushes and closes the history file.
func (h *History) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		h.file.Close()
	}
}

// Start records that a job has begun execution.
func (h *History) Start(msg Message, model string, useLocal bool, queue string) *JobRecord {
	rec := JobRecord{
		ID:        msg.ID,
		Type:      msg.Type,
		Source:    msg.Source,
		Model:     model,
		UseLocal:  useLocal,
		Queue:     queue,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "running",
	}
	h.write(rec)
	return &rec
}

// Complete records that a job finished successfully.
func (h *History) Complete(rec *JobRecord) {
	h.mu.Lock()
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	start, _ := time.Parse(time.RFC3339, rec.StartedAt)
	rec.Duration = time.Since(start).Round(time.Millisecond).String()
	rec.Status = "completed"
	h.mu.Unlock()
	h.write(*rec)
	log.Printf("history: job %s completed in %s (model=%s)", rec.ID, rec.Duration, rec.Model)
}

// Fail records that a job failed.
func (h *History) Fail(rec *JobRecord, err error) {
	h.mu.Lock()
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	start, _ := time.Parse(time.RFC3339, rec.StartedAt)
	rec.Duration = time.Since(start).Round(time.Millisecond).String()
	rec.Status = "failed"
	if err != nil {
		rec.Error = err.Error()
	}
	h.mu.Unlock()
	h.write(*rec)
	log.Printf("history: job %s failed after %s: %v", rec.ID, rec.Duration, err)
}

// Denied records that a job was blocked by the approval gate due to cost.
func (h *History) Denied(rec *JobRecord, estimatedCost float64) {
	h.mu.Lock()
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	start, _ := time.Parse(time.RFC3339, rec.StartedAt)
	rec.Duration = time.Since(start).Round(time.Millisecond).String()
	rec.Status = "denied"
	rec.EstimatedCost = estimatedCost
	rec.Error = "estimated cost exceeds threshold"
	h.mu.Unlock()
	h.write(*rec)
	log.Printf("history: job %s denied (estimated cost=$%.4f)", rec.ID, estimatedCost)
}

func (h *History) write(rec JobRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := json.Marshal(rec)
	if err != nil {
		log.Printf("history: marshal record %s: %v", rec.ID, err)
		return
	}
	_, err = h.file.Write(append(data, '\n'))
	if err != nil {
		log.Printf("history: write record %s: %v", rec.ID, err)
	}
}

// Path returns the path to the jobs JSONL file.
func (h *History) Path() string {
	return filepath.Join(h.dir, "jobs.jsonl")
}
