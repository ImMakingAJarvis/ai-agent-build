// Package dashboard: the interactive queue for pending agent jobs.
//
// The queue is the set of ready agent jobs, grouped into the three bands
// (fix, build, background), shown in the current runtime band priority order
// (agentjob.Store.BandOrder; default fix, build, background). A job can be
// reordered within its band or dropped into another band to reclassify it,
// and the band order itself can be changed at runtime (POST
// /api/queue/band-order). All of this is persisted by rewriting the job's
// class and within-class priority (agentjob.Store.ApplyOrder) or the band
// order (agentjob.Store.SetBandOrder), which is exactly what the run order
// (Store.List / Store.Next) is derived from.
package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

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

// queueResponse is the body of GET /api/queue, POST /api/queue/reorder and
// POST /api/queue/band-order.
type queueResponse struct {
	// Order is the current runtime band priority (highest first); the bands
	// below are in the same order.
	Order []string    `json:"order"`
	Bands []queueBand `json:"bands"`
}

// openAgentJobStore opens the agent job store the dashboard reads from.
func (s *Server) openAgentJobStore() (*agentjob.Store, error) {
	return agentjob.Open(filepath.Dir(s.historyPath))
}

// loadQueue lists the ready jobs in run order and groups them by band. Bands
// are always present (even empty) and in the current runtime band order. A
// ready job is marked blocked when one of its dependencies is not done yet.
func (s *Server) loadQueue(st *agentjob.Store) (bands []queueBand, order []string, err error) {
	jobs, err := st.List()
	if err != nil {
		return nil, nil, err
	}
	order, err = st.BandOrder()
	if err != nil {
		return nil, nil, err
	}
	status := make(map[string]string, len(jobs))
	for _, j := range jobs {
		status[j.ID] = j.Status
	}

	byBand := make(map[string][]queueJob, len(order))
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

	bands = make([]queueBand, 0, len(order))
	for _, cls := range order {
		bands = append(bands, queueBand{Name: cls, Jobs: byBand[cls]})
	}
	return bands, order, nil
}

// handleQueue returns the pending (ready) queue grouped by band in the
// current runtime band order.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	st, err := s.openAgentJobStore()
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	bands, order, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Order: order, Bands: bands})
}

// handleQueueBandOrder changes the runtime band priority order. The body is
// {"order": ["build", "fix", "background"]} — any permutation of the three
// bands, highest priority first. It is persisted in the job store and takes
// effect immediately: the queue view and the worker run order (Store.Next)
// are both re-sorted. The updated queue is returned for re-render.
func (s *Server) handleQueueBandOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Order []string `json:"order"`
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
	if err := st.SetBandOrder(req.Order); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bands, order, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Order: order, Bands: bands})
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
	order, err := st.BandOrder()
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

	orders := make(map[string][]string, len(order))
	seen := make(map[string]int)
	for _, cls := range order {
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
	bands, order, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Order: order, Bands: bands})
}
