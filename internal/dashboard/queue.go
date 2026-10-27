// Package dashboard: the interactive queue for pending agent jobs.
//
// The queue is the set of ready agent jobs, grouped into the three locked
// bands (fix, build, background). The bands never change order; a job can be
// reordered within its band or dropped into another band to reclassify it.
// Both actions are persisted by rewriting the job's class and within-class
// priority (agentjob.Store.ApplyOrder), which is exactly what the run order
// (Store.List / Store.Next) is derived from.
package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// queueBandOrder is the locked run order of the bands: fix first, then build,
// then background. Dragging cannot change it, only the order within a band.
var queueBandOrder = []string{agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassBackground}

// queueJob is one pending job as shown in the interactive queue.
type queueJob struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Class     string   `json:"class"`
	Priority  int      `json:"priority"`
	CreatedAt string   `json:"created_at"`
	Driver    string   `json:"driver"`
	Status    string   `json:"status"`
	DependsOn []string `json:"depends_on,omitempty"`
	Blocked   bool     `json:"blocked"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// queueBand is one locked band and the ready jobs it holds, top-to-bottom in
// run order (the first job runs first).
type queueBand struct {
	Name string     `json:"name"`
	Jobs []queueJob `json:"jobs"`
}

// queueResponse is the body of GET /api/queue and POST /api/queue/reorder.
type queueResponse struct {
	Bands []queueBand `json:"bands"`
}

// openAgentJobStore opens the agent job store the dashboard reads from.
func (s *Server) openAgentJobStore() (*agentjob.Store, error) {
	return agentjob.Open(filepath.Dir(s.historyPath))
}

// loadQueue lists the ready jobs in run order and groups them by band. Bands
// are always present (even empty) and always in the locked order. A ready
// job is marked blocked when one of its dependencies is not done yet.
func (s *Server) loadQueue(st *agentjob.Store) ([]queueBand, error) {
	jobs, err := st.List()
	if err != nil {
		return nil, err
	}
	status := make(map[string]string, len(jobs))
	for _, j := range jobs {
		status[j.ID] = j.Status
	}

	byBand := make(map[string][]queueJob, len(queueBandOrder))
	for _, j := range jobs {
		if j.Status != agentjob.Ready {
			continue
		}
		var blockedBy []string
		for _, d := range j.DependsOn {
			if status[d] != agentjob.Done {
				blockedBy = append(blockedBy, d)
			}
		}
		cls := agentjob.EffectiveClass(j.Class, j.Source)
		qj := queueJob{
			ID:        j.ID,
			Title:     j.Title,
			Class:     cls,
			Priority:  j.Priority,
			CreatedAt: j.CreatedAt.UTC().Format("2006-01-02 15:04"),
			Driver:    j.Driver,
			Status:    j.Status,
			DependsOn: j.DependsOn,
			Blocked:   len(blockedBy) > 0,
			BlockedBy: blockedBy,
		}
		byBand[cls] = append(byBand[cls], qj)
	}

	bands := make([]queueBand, 0, len(queueBandOrder))
	for _, cls := range queueBandOrder {
		bands = append(bands, queueBand{Name: cls, Jobs: byBand[cls]})
	}
	return bands, nil
}

// handleQueue returns the pending (ready) queue grouped by band in run order.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	st, err := s.openAgentJobStore()
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	bands, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Bands: bands})
}

// handleQueueReorder persists a new queue arrangement. The client sends the
// full desired order of the ready jobs, grouped by band. Every ready job must
// appear exactly once; each job's class and within-class priority are then
// rewritten to match, and the updated queue is returned for re-render.
func (s *Server) handleQueueReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Orders map[string][]string `json:"orders"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	st, err := s.openAgentJobStore()
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	jobs, err := st.List()
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ready := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		if j.Status == agentjob.Ready {
			ready[j.ID] = true
		}
	}

	orders := make(map[string][]string, len(queueBandOrder))
	seen := make(map[string]int)
	for _, cls := range queueBandOrder {
		for _, id := range req.Orders[cls] {
			if !ready[id] {
				http.Error(w, "job "+id+" is not in the queue", http.StatusConflict)
				return
			}
			seen[id]++
			orders[cls] = append(orders[cls], id)
		}
	}
	// Every ready job must be listed exactly once: no missing, no duplicates.
	for id := range ready {
		if seen[id] != 1 {
			http.Error(w, "job "+id+" must be listed exactly once", http.StatusConflict)
			return
		}
	}

	if err := st.ApplyOrder(orders); err != nil {
		http.Error(w, "reorder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bands, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Bands: bands})
}
