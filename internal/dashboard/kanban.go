// Package dashboard kanban handlers.
package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
)

// kanbanState holds the kanban store for the dashboard.
type kanbanState struct {
	store *kanban.Store
}

// initKanban opens the kanban store.
func initKanban(dataDir string) *kanbanState {
	store, err := kanban.Open(dataDir)
	if err != nil {
		panic("kanban store: " + err.Error())
	}
	return &kanbanState{store: store}
}

// --- Request/Response types ---

type createEpicReq struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

type createTaskReq struct {
	EpicID   string `json:"epic_id"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Priority int    `json:"priority"`
	Model    string `json:"model"`
}

type createSubTaskReq struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
}

type updateTaskReq struct {
	Status   string `json:"status,omitempty"`
	Priority *int   `json:"priority,omitempty"`
	Model    string `json:"model,omitempty"`
	Title    string `json:"title,omitempty"`
}

// modelOption represents a selectable model in the kanban board dropdowns.
type modelOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// handleKanbanBoardPage renders the kanban board page.
func (ks *kanbanState) handleKanbanBoardPage(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := s.tmpl.ExecuteTemplate(&buf, "kanban.html", nil); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w)
	}
}

// --- Epic API ---

func (ks *kanbanState) apiCreateEpic(w http.ResponseWriter, r *http.Request) {
	var req createEpicReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	ep := ks.store.AddEpic(req.Title, req.Description)
	respondJSON(w, ep)
}

func (ks *kanbanState) apiListEpics(w http.ResponseWriter, r *http.Request) {
	epics, err := ks.store.ListEpics()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, epics)
}

func (ks *kanbanState) apiDeleteEpic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := ks.store.DeleteEpic(id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (ks *kanbanState) apiUpdateEpic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Title != "" {
		if err := ks.store.UpdateEpicTitle(id, req.Title); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	ep, err := ks.store.GetEpic(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	respondJSON(w, ep)
}

// --- Task API ---

func (ks *kanbanState) apiCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Title == "" || req.EpicID == "" {
		http.Error(w, "title and epic_id are required", http.StatusBadRequest)
		return
	}
	t := ks.store.AddTask(req.EpicID, req.Title, req.Content, req.Priority)
	if req.Model != "" {
		ks.store.UpdateTaskModel(t.ID, req.Model)
		// Re-read to get the model field populated.
		t, _ = ks.store.GetTask(t.ID)
	}
	respondJSON(w, t)
}

func (ks *kanbanState) apiListTasks(w http.ResponseWriter, r *http.Request) {
	epicID := r.URL.Query().Get("epic_id")
	tasks, err := ks.store.ListTasks()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if epicID != "" {
		// Non-nil so an empty result marshals as [] and never as null.
		filtered := make([]kanban.Task, 0, len(tasks))
		for _, t := range tasks {
			if t.EpicID == epicID {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}
	respondJSON(w, tasks)
}

func (ks *kanbanState) apiDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := ks.store.DeleteTask(id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (ks *kanbanState) apiUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updateTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Status != "" {
		if err := ks.store.UpdateTaskStatus(id, kanban.Status(req.Status)); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if req.Priority != nil {
		if err := ks.store.UpdateTaskPriority(id, *req.Priority); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if req.Model != "" {
		if err := ks.store.UpdateTaskModel(id, req.Model); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if req.Title != "" {
		if err := ks.store.UpdateTaskTitle(id, req.Title); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	t, err := ks.store.GetTask(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	respondJSON(w, t)
}

// --- SubTask API ---

func (ks *kanbanState) apiCreateSubTask(w http.ResponseWriter, r *http.Request) {
	var req createSubTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Title == "" || req.TaskID == "" {
		http.Error(w, "title and task_id are required", http.StatusBadRequest)
		return
	}
	st := ks.store.AddSubTask(req.TaskID, req.Title)
	respondJSON(w, st)
}

func (ks *kanbanState) apiToggleSubTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := ks.store.ToggleSubTask(id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Return 204
	w.WriteHeader(http.StatusNoContent)
}

func (ks *kanbanState) apiListSubTasks(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		http.Error(w, "task_id query param required", http.StatusBadRequest)
		return
	}
	subtasks, err := ks.store.ListSubTasks(taskID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, subtasks)
}

// --- Helper ---

// parseJSONBody decodes a JSON body into v.
func parseJSONBody(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// buildModelOptions constructs the list of models available for task assignment.
// API models (Sonnet, Opus, Haiku, Fable) are always included.
// Local models are read from environment variables and deduplicated.
func buildModelOptions() []modelOption {
	opts := []modelOption{
		{Value: "sonnet", Label: "Sonnet"},
		{Value: "opus", Label: "Opus"},
		{Value: "haiku", Label: "Haiku"},
		{Value: "fable", Label: "Fable"},
	}

	// Collect local model names from env vars.
	localNames := []struct {
		env   string
		label string
	}{
		{os.Getenv("LOCAL_MODEL"), "Local"},
		{os.Getenv("LOCAL_LIGHT_MODEL"), "Local (Light)"},
		{os.Getenv("LOCAL_CODER_MODEL"), "Local (Coder)"},
	}

	seen := make(map[string]struct{})
	for _, m := range localNames {
		if m.env == "" {
			continue
		}
		if _, ok := seen[m.env]; ok {
			continue
		}
		seen[m.env] = struct{}{}
		opts = append(opts, modelOption{Value: m.env, Label: m.label})
	}

	// If no local models configured at all, fall back to generic "local".
	if len(seen) == 0 {
		opts = append(opts, modelOption{Value: "local", Label: "Local"})
	}

	return opts
}

// apiListModels returns the available model options for task assignment.
func (ks *kanbanState) apiListModels(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, buildModelOptions())
}
