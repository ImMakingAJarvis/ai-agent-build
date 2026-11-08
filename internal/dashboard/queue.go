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
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
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
	// Photo is true for photo-tagged jobs: they are shown in the dedicated
	// photo band and their editable text is the generation prompt (Prompt),
	// not the generic instructions.
	Photo        bool     `json:"photo,omitempty"`
	Instructions string   `json:"instructions,omitempty"` // editable text of the work
	Prompt       string   `json:"prompt,omitempty"`       // photo job: the sidecar prompt
	// InFlight is true while the job's round is running (claimed/running).
	// Only photo-tagged jobs are shown in flight (in the photo band, ahead
	// of the ready batch): the card is not draggable or editable and the
	// page's Log link opens the round's live log.
	InFlight     bool     `json:"in_flight,omitempty"`
	StartedAt    string   `json:"started_at,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
	Blocked      bool     `json:"blocked"`
	BlockedBy    []string `json:"blocked_by,omitempty"`
}

// PhotoBand is the name of the display-only band for photo-tagged jobs. It
// is always rendered first (photo work is the most time-sensitive: it waits
// for the brain-swap window) and is not part of the runtime band order —
// photo jobs keep their own class (fix for !photonow, build for !photo) and
// priority, which is what actually orders their run.
const PhotoBand = "photo"

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

// queueTime re-formats an RFC3339 timestamp from state.json for card
// display, matching the CreatedAt format; an empty or unparseable value is
// returned as-is.
func queueTime(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04")
	}
	return s
}

// loadQueue lists the ready jobs in run order and groups them by band. Bands
// are always present (even empty) and in the current runtime band order. A
// ready job is marked blocked when one of its dependencies is not done yet.
// Photo-tagged jobs whose round is running (claimed/running) are shown in
// flight at the head of the photo band: photo work runs on the iGPU in the
// brain-swap window, so it is the queue's only in-progress work (the rest is
// covered by the Agent Jobs panel).
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
	var photoInFlight, photoJobs []queueJob
	for _, j := range jobs {
		if (j.Status == agentjob.Claimed || j.Status == agentjob.Running) && j.HasTag(photo.Tag) {
			qj := queueJob{
				ID:        j.ID,
				Title:     j.Title,
				Class:     agentjob.EffectiveClass(j.Class, j.Source),
				Priority:  j.Priority,
				CreatedAt: j.CreatedAt.UTC().Format("2006-01-02 15:04"),
				Driver:    j.Driver,
				Status:    j.Status,
				Photo:     true,
				InFlight:  true,
				StartedAt: queueTime(j.StartedAt),
			}
			if p, err := photo.ReadPrompt(st.Dir(), j.ID); err == nil {
				qj.Prompt = p
			}
			photoInFlight = append(photoInFlight, qj)
			continue
		}
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
			ID:           j.ID,
			Title:        j.Title,
			Class:        cls,
			Priority:     j.Priority,
			CreatedAt:    j.CreatedAt.UTC().Format("2006-01-02 15:04"),
			Driver:       j.Driver,
			Status:       j.Status,
			Instructions: j.Instructions,
			DependsOn:    j.DependsOn,
			Blocked:      len(blockedBy) > 0,
			BlockedBy:    blockedBy,
		}
		if j.HasTag(photo.Tag) {
			// The photo's editable text is the brain-expanded prompt in the
			// sidecar file the iGPU round reads (jarvis-photo), not the
			// generic job instructions in job.json.
			if p, err := photo.ReadPrompt(st.Dir(), j.ID); err == nil {
				qj.Prompt = p
			}
			qj.Photo = true
			photoJobs = append(photoJobs, qj)
			continue
		}
		byBand[cls] = append(byBand[cls], qj)
	}

	// The photo band comes first and is outside the runtime band order: it
	// is a display grouping only, so Order (and the run order) is unchanged.
	// In-flight (running) photo jobs are listed ahead of the ready batch.
	bands = make([]queueBand, 0, len(order)+1)
	bands = append(bands, queueBand{Name: PhotoBand, Jobs: append(append([]queueJob{}, photoInFlight...), photoJobs...)})
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
	// Ready jobs that must be listed exactly once. Photo-tagged jobs sit in
	// the display-only photo band, so they are exempt: the client may list
	// one under a class band (reclassifying it, as before the photo band
	// existed) or omit it entirely. The "photo" key itself is ignored — the
	// loop below only reads the three runtime classes.
	ready := make(map[string]bool, len(jobs))
	photoReady := make(map[string]bool)
	for _, j := range jobs {
		if j.Status != agentjob.Ready {
			continue
		}
		if j.HasTag(photo.Tag) {
			photoReady[j.ID] = true
		} else {
			ready[j.ID] = true
		}
	}

	orders := make(map[string][]string, len(order))
	seen := make(map[string]int)
	for _, cls := range order {
		for _, id := range req.Orders[cls] {
			if !ready[id] && !photoReady[id] {
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

// handleQueueEdit edits the text of a queued (ready) job: its title and its
// instructions (the body of the work). A photo-tagged job's editable text is
// the generation prompt, which lives in the sidecar file the iGPU round
// reads — so for a photo job the instructions field updates that file and
// the generic instructions in job.json are left as-is. Only ready jobs can
// be edited (the queue UI only shows ready jobs). The updated queue is
// returned for re-render.
func (s *Server) handleQueueEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !agentjob.ValidID(req.ID) {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	st, err := s.openAgentJobStore()
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	j, err := st.Get(req.ID)
	if err != nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	if j.Status != agentjob.Ready {
		http.Error(w, "job "+req.ID+" is "+j.Status+", not ready", http.StatusConflict)
		return
	}

	isPhoto := j.HasTag(photo.Tag)
	instr := req.Instructions
	if isPhoto {
		// Photo text goes to the prompt sidecar; job.json keeps the generic
		// "run jarvis-photo make --job <id>" instructions the iGPU round acts on.
		instr = ""
	}
	if req.Title == "" && instr == "" {
		http.Error(w, "nothing to edit (title and instructions both empty)", http.StatusBadRequest)
		return
	}
	if err := st.Edit(req.ID, req.Title, instr); err != nil {
		http.Error(w, "edit: "+err.Error(), http.StatusBadRequest)
		return
	}
	if isPhoto && req.Instructions != "" {
		if err := photo.WritePrompt(st.Dir(), req.ID, req.Instructions); err != nil {
			http.Error(w, "write prompt: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	bands, order, err := s.loadQueue(st)
	if err != nil {
		http.Error(w, "load queue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, queueResponse{Order: order, Bands: bands})
}
