// Package api is loopd's HTTP layer (M4.1). This file (card ELR-95,
// M4.1c) adds the write side: create a task, cancel it, and add a user
// message to its conversation. The approval endpoints moved to
// approvals.go with card m4-3d (ELR-109).
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// TaskRequest is the body of POST /v1/tasks: one task for the runtime.
type TaskRequest struct {
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Branch       string   `json:"branch"`
	Workspace    string   `json:"workspace"`
	Acceptance   []string `json:"acceptance"`
	DependsOn    []string `json:"depends_on"`
}

// ErrConflict is wrapped by a Submit implementation when the task cannot
// be created because it conflicts with the store's state (card m4-2d
// adds the submit checks that produce it); the API answers 409.
var ErrConflict = errors.New("api: conflict")

// defaultSubmit is the Deps.Submit default (M4.1c): the task is added to
// the agentjob store as a ready loop job, then handed to the Submitter —
// the loop's Submit, wired by cmd/loopd — so the runtime picks it up. A
// nil Submitter (a Server built without a loop) leaves the job ready in
// the store. If the Submitter fails after the Ingest, the job stays ready
// and the error is returned: the Ingest is idempotent, so a retry cannot
// create a duplicate.
func (d Deps) defaultSubmit(req TaskRequest) (string, error) {
	id, err := newTaskID()
	if err != nil {
		return "", err
	}
	if _, err := d.Store.Ingest(agentjob.Spec{
		ID:           id,
		Title:        req.Title,
		Instructions: req.Instructions,
		Branch:       req.Branch,
		Workspace:    req.Workspace,
		Acceptance:   req.Acceptance,
		DependsOn:    req.DependsOn,
		Driver:       "loop",
		Class:        agentjob.ClassBuild,
		Source:       "api",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	if d.Submitter != nil {
		if err := d.Submitter(id); err != nil {
			return "", err
		}
	}
	return id, nil
}

// newTaskID builds a task id: "task-" + unix seconds + 8 hex chars of
// crypto/rand, so two requests in the same second cannot collide.
func newTaskID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("api: task id: %w", err)
	}
	return fmt.Sprintf("task-%d-%s", time.Now().Unix(), hex.EncodeToString(b[:])), nil
}

// handleCreateTask serves POST /v1/tasks: it creates the task through
// Deps.Submit. 201 {"id":..} on success; 400 on bad JSON or empty
// instructions; 409 when Submit returns an error wrapping ErrConflict
// (card m4-2d adds those checks); 500 on any other Submit error.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	if strings.TrimSpace(req.Instructions) == "" {
		httpError(w, http.StatusBadRequest, "instructions must not be empty")
		return
	}
	id, err := s.deps.Submit(req)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpError(w, http.StatusConflict, "conflict: %v", err)
			return
		}
		httpError(w, http.StatusInternalServerError, "submit: %v", err)
		return
	}
	b, err := s.redacted(map[string]any{"id": id})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "encode: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write(b); err != nil {
		// The client went away; there is nothing left to do.
	}
}

// handleCancel serves POST /v1/tasks/{id}/cancel: it cancels every
// running op of the job through the Runner and sets the job status to
// stopped — the store's parked status (the plan's "stopped/parked"): the
// job is out of the active queue, and SetStatus's dependency sweep
// dep-blocks its ready dependents. Unknown id is 404; a second cancel is
// a 200 no-op, as is a cancel of a finished job — a cancel that lands
// after a clean finish must not park a done, failed or skipped job
// (Store.Stop's rule).
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.deps.Store.Get(id)
	if err != nil {
		notFound(w)
		return
	}
	if s.deps.Runner != nil {
		jobOps, err := s.deps.Store.Ops(id)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "ops: %v", err)
			return
		}
		for _, op := range jobOps {
			if op.Status == agentjob.OpRunning {
				s.deps.Runner.Cancel(op.ID)
			}
		}
	}
	status := job.Status
	switch status {
	case agentjob.Parked, agentjob.Done, agentjob.Failed, agentjob.Skipped:
		// Already stopped or finished: the cancel changes nothing.
	default:
		st := job.State
		st.Status = agentjob.Parked
		if err := s.deps.Store.SetStatus(id, st); err != nil {
			httpError(w, http.StatusInternalServerError, "set status: %v", err)
			return
		}
		status = agentjob.Parked
	}
	s.writeJSON(w, map[string]any{"id": id, "status": status})
}

// handleAddMessage serves POST /v1/tasks/{id}/messages: {"text":..} is
// appended to the job's conversation log (messages.jsonl) as a user
// message — the same shape loop.Submit records the instructions with.
// 200 with the message's seq; 400 on bad JSON or empty text; 404 for an
// unknown job (appending would create a phantom job directory).
func (s *Server) handleAddMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		httpError(w, http.StatusBadRequest, "text must not be empty")
		return
	}
	id := r.PathValue("id")
	if _, err := s.deps.Store.Get(id); err != nil {
		notFound(w)
		return
	}
	content, err := json.Marshal(body.Text)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "encode text: %v", err)
		return
	}
	msgs, err := s.deps.Store.AppendMessages(id, agentjob.LoopMessage{
		Role:    "user",
		Content: content,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "append message: %v", err)
		return
	}
	s.writeJSON(w, map[string]any{"seq": msgs[0].Seq})
}
