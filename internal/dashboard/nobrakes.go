// Package dashboard NoBrakes handlers (5/6): the audit log of every job
// the nightly gate auto-enqueued, surfaced read-only.
package dashboard

import (
	"net/http"
	"path/filepath"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// nobrakesAuditResp is GET /api/nobrakes/audit: every auto-execution audit
// entry (oldest first) plus the log file they came from. The dashboard only
// reads the durable JSONL the gate appends to — it never writes it.
type nobrakesAuditResp struct {
	Entries []nobrakes.AuditEntry `json:"entries"`
	Path    string                `json:"path"`
}

// handleNobrakesAudit serves the audit log. A missing log file is an empty
// list, not an error: NoBrakes simply has not auto-run anything yet.
func (s *Server) handleNobrakesAudit(w http.ResponseWriter, r *http.Request) {
	path := nobrakes.DefaultAuditPath(filepath.Dir(s.historyPath))
	entries, err := nobrakes.RecentAudit(path, 0)
	if err != nil {
		http.Error(w, "audit log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, nobrakesAuditResp{Entries: entries, Path: path})
}
