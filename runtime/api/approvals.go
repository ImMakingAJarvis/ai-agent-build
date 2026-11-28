// approvals.go is the approval endpoints (card ELR-109, M4.3d): the
// owner's decision on an approval parked by the permission gate (card
// ELR-107) — recorded with Store.DecideApproval, acted on through the
// loop, and put on the event stream — plus the list of a job's
// approvals. The bearer token is the API owner identity: a decided_by
// that names a Discord id other than the configured owner is refused
// (403) before anything is written.
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// approvalRequest is the body of POST /v1/tasks/{id}/approvals (card
// ELR-109): the decision on the parked approval named by approval_id.
// hash must equal the approval's stored hash — the exact call it
// covers — and decision is "approve" or "deny".
type approvalRequest struct {
	ApprovalID string `json:"approval_id"`
	Hash       string `json:"hash"`
	Decision   string `json:"decision"`
	Note       string `json:"note"`
	DecidedBy  string `json:"decided_by"`
}

// handleApprovals serves POST /v1/tasks/{id}/approvals (card ELR-109,
// M4.3d): it records the decision with Store.DecideApproval and, when
// the decision is new (changed), acts on it through OnApprovalDecided —
// the loop's HandleApprovalDecided, wired by cmd/loopd — and publishes
// approval.decided on the Broker. A decided_by of the form
// discord:<id> that is not the configured owner is 403 and changes
// nothing (the bearer token is the owner identity, and it cannot speak
// as someone else). 200 {"changed":true}; 200 {"changed":false} when
// the approval already carried a decision; 400 on a hash mismatch
// (ErrApprovalHashMismatch), a bad decision or a missing approval_id;
// 409 after the task lapsed (ErrApprovalClosed: the job is done, failed
// or stopped); 404 for an unknown job or approval.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.deps.Store.Get(id); err != nil {
		notFound(w)
		return
	}
	var req approvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	if req.Decision != "approve" && req.Decision != "deny" {
		httpError(w, http.StatusBadRequest, "decision must be approve or deny")
		return
	}
	if req.ApprovalID == "" {
		httpError(w, http.StatusBadRequest, "approval_id is required")
		return
	}
	if strings.HasPrefix(req.DecidedBy, "discord:") && !s.ownerDiscord(req.DecidedBy) {
		httpError(w, http.StatusForbidden, "decided_by is not the owner's discord id")
		return
	}
	changed, err := s.deps.Store.DecideApproval(id, req.ApprovalID, req.Hash, req.DecidedBy, req.Note, req.Decision == "approve")
	if err != nil {
		switch {
		case errors.Is(err, agentjob.ErrApprovalClosed):
			httpError(w, http.StatusConflict, "approval lapsed: %v", err)
		case errors.Is(err, agentjob.ErrApprovalHashMismatch):
			httpError(w, http.StatusBadRequest, "hash mismatch: %v", err)
		case errors.Is(err, fs.ErrNotExist):
			notFound(w)
		default:
			httpError(w, http.StatusInternalServerError, "decide approval: %v", err)
		}
		return
	}
	if !changed {
		s.writeJSON(w, map[string]any{"changed": false})
		return
	}
	if s.deps.OnApprovalDecided != nil {
		if err := s.deps.OnApprovalDecided(id, req.ApprovalID); err != nil {
			httpError(w, http.StatusInternalServerError, "act on decision: %v", err)
			return
		}
	}
	// The loop's approvals are named after the op they cover
	// (parkForApproval sets id and op_id alike), so the event can point
	// at the op.
	status := agentjob.ApprovalDenied
	if req.Decision == "approve" {
		status = agentjob.ApprovalApproved
	}
	data, _ := json.Marshal(map[string]any{"status": status})
	if s.deps.Broker != nil {
		s.deps.Broker.Publish(Event{Type: "approval.decided", JobID: id, OpID: req.ApprovalID, Data: data})
	}
	s.writeJSON(w, map[string]any{"changed": true})
}

// ownerDiscord reports whether a decided_by names the configured owner:
// the owner_discord_id from the loopd config, in the bare form or the
// API's discord:<id> form. A server without a configured owner accepts
// no discord:<id> — the bearer token alone is the owner identity.
func (s *Server) ownerDiscord(by string) bool {
	owner := s.deps.OwnerDiscordID
	if owner == "" {
		return false
	}
	return by == owner || by == "discord:"+owner
}

// handleListApprovals serves GET /v1/tasks/{id}/approvals (card
// ELR-109): every approval of the job, request text through the
// Redactor (writeJSON applies it to every string, the ones inside the
// raw request included). A job with no approvals yet is an empty
// array; an unknown job is 404.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.deps.Store.Get(id); err != nil {
		notFound(w)
		return
	}
	aps, err := s.deps.Store.Approvals(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "approvals: %v", err)
		return
	}
	if aps == nil {
		aps = []agentjob.LoopApproval{}
	}
	s.writeJSON(w, aps)
}
