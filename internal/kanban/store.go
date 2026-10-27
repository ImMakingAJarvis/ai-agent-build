package kanban

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store persists epics, tasks, and subtasks to a directory tree.
//
// Layout:
//
//	<root>/epics/<epic-id>.json
//	<root>/tasks/<task-id>.json
//	<root>/subtasks/<subtask-id>.json
//
// All mutations are protected by a mutex and written atomically
// (write to .tmp, then rename).
type Store struct {
	mu   sync.Mutex
	root string
}

// DataDir returns JARVIS_DATA_DIR or ~/.local/share/jarvis.
func DataDir() string {
	if d := os.Getenv("JARVIS_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return filepath.Join(home, ".local", "share", "jarvis")
}

// Open creates (or loads) a store under the kanban/ subdirectory of dir.
func Open(dir string) (*Store, error) {
	root := filepath.Join(dir, "kanban")
	for _, sub := range []string{"epics", "tasks", "subtasks"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("kanban: mkdir %s: %w", sub, err)
		}
	}
	return &Store{root: root}, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- Epics ---

// AddEpic creates a new epic and returns it.
func (s *Store) AddEpic(title, description string) Epic {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep := Epic{
		ID:          uuid.New().String(),
		Title:       title,
		Description: description,
		CreatedAt:   time.Now().UTC(),
	}
	path := filepath.Join(s.root, "epics", ep.ID+".json")
	if err := writeJSON(path, ep); err != nil {
		// Log but don't fail — caller can retry.
		fmt.Printf("kanban: write epic: %v\n", err)
	}
	return ep
}

// GetEpic loads an epic by ID.
func (s *Store) GetEpic(id string) (Epic, error) {
	var ep Epic
	b, err := os.ReadFile(filepath.Join(s.root, "epics", id+".json"))
	if err != nil {
		return ep, err
	}
	return ep, json.Unmarshal(b, &ep)
}

// ListEpics returns all epics sorted by creation time descending.
func (s *Store) ListEpics() ([]Epic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(filepath.Join(s.root, "epics"))
	if err != nil {
		return nil, err
	}
	// Non-nil so an empty board marshals as [] and never as null.
	epics := make([]Epic, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			b, err := os.ReadFile(filepath.Join(s.root, "epics", e.Name()))
			if err != nil {
				continue
			}
			var ep Epic
			if err := json.Unmarshal(b, &ep); err != nil {
				continue
			}
			epics = append(epics, ep)
		}
	}
	sort.Slice(epics, func(i, j int) bool {
		return epics[i].CreatedAt.After(epics[j].CreatedAt)
	})
	return epics, nil
}

// UpdateEpicTitle changes an epic's title.
func (s *Store) UpdateEpicTitle(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "epics", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var ep Epic
	if err := json.Unmarshal(b, &ep); err != nil {
		return err
	}
	ep.Title = title
	return writeJSON(path, ep)
}

// DeleteEpic removes an epic and all of its tasks and their subtasks.
func (s *Store) DeleteEpic(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep, err := s.unlockedGetEpic(id)
	if err != nil {
		return err
	}
	taskIDs := make(map[string]bool, len(ep.TaskIDs))
	for _, tid := range ep.TaskIDs {
		taskIDs[tid] = true
		os.Remove(filepath.Join(s.root, "tasks", tid+".json"))
	}
	if sts, err := s.unlockedListSubTasks(); err == nil {
		for _, st := range sts {
			if taskIDs[st.TaskID] {
				os.Remove(filepath.Join(s.root, "subtasks", st.ID+".json"))
			}
		}
	}
	return os.Remove(filepath.Join(s.root, "epics", id+".json"))
}

// --- Tasks ---

// AddTask creates a new task and adds it to the epic's task list.
func (s *Store) AddTask(epicID, title, content string, priority int) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	if priority < 0 {
		priority = 0
	}
	if priority == 0 {
		priority = 5
	}
	now := time.Now().UTC()
	t := Task{
		ID:        uuid.New().String(),
		EpicID:    epicID,
		Title:     title,
		Content:   content,
		Status:    StatusBacklog,
		Priority:  priority,
		CreatedAt: now,
		UpdatedAt: now,
	}
	path := filepath.Join(s.root, "tasks", t.ID+".json")
	if err := writeJSON(path, t); err != nil {
		fmt.Printf("kanban: write task: %v\n", err)
		return t
	}
	// Append task ID to the epic.
	ep, err := s.unlockedGetEpic(epicID)
	if err == nil {
		ep.TaskIDs = append(ep.TaskIDs, t.ID)
		epPath := filepath.Join(s.root, "epics", epicID+".json")
		writeJSON(epPath, ep)
	}
	return t
}

// GetTask loads a task by ID.
func (s *Store) GetTask(id string) (Task, error) {
	var t Task
	b, err := os.ReadFile(filepath.Join(s.root, "tasks", id+".json"))
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(b, &t)
}

// ListTasks returns all tasks sorted by priority descending, then creation time.
func (s *Store) ListTasks() ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(filepath.Join(s.root, "tasks"))
	if err != nil {
		return nil, err
	}
	// Non-nil so an empty board marshals as [] and never as null.
	tasks := make([]Task, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			b, err := os.ReadFile(filepath.Join(s.root, "tasks", e.Name()))
			if err != nil {
				continue
			}
			var t Task
			if err := json.Unmarshal(b, &t); err != nil {
				continue
			}
			tasks = append(tasks, t)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority > tasks[j].Priority
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	return tasks, nil
}

// UpdateTaskStatus changes a task's status.
func (s *Store) UpdateTaskStatus(id string, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	t.Status = status
	t.UpdatedAt = time.Now().UTC()
	return writeJSON(path, t)
}

// UpdateTaskBranch sets the branch name on a task.
func (s *Store) UpdateTaskBranch(id, branch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	t.Branch = branch
	t.UpdatedAt = time.Now().UTC()
	return writeJSON(path, t)
}

// UpdateTaskTitle changes a task's title.
func (s *Store) UpdateTaskTitle(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UTC()
	return writeJSON(path, t)
}

// UpdateTaskPriority changes a task's priority (0 is treated as the default 5).
func (s *Store) UpdateTaskPriority(id string, priority int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	if priority < 0 {
		priority = 0
	}
	if priority == 0 {
		priority = 5
	}
	t.Priority = priority
	t.UpdatedAt = time.Now().UTC()
	return writeJSON(path, t)
}

// UpdateTaskModel sets the model name on a task.
func (s *Store) UpdateTaskModel(id, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	t.Model = model
	t.UpdatedAt = time.Now().UTC()
	return writeJSON(path, t)
}

// DeleteTask removes a task file, its subtasks, and unlinks it from its epic.
func (s *Store) DeleteTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "tasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	if sts, err := s.unlockedListSubTasks(); err == nil {
		for _, st := range sts {
			if st.TaskID == id {
				os.Remove(filepath.Join(s.root, "subtasks", st.ID+".json"))
			}
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// Unlink from the epic.
	if ep, err := s.unlockedGetEpic(t.EpicID); err == nil {
		kept := make([]string, 0, len(ep.TaskIDs))
		for _, tid := range ep.TaskIDs {
			if tid != id {
				kept = append(kept, tid)
			}
		}
		ep.TaskIDs = kept
		writeJSON(filepath.Join(s.root, "epics", t.EpicID+".json"), ep)
	}
	return nil
}

// --- SubTasks ---

// AddSubTask creates a subtask checklist item for a task.
func (s *Store) AddSubTask(taskID, title string) SubTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SubTask{
		ID:        uuid.New().String(),
		TaskID:    taskID,
		Title:     title,
		CreatedAt: time.Now().UTC(),
	}
	path := filepath.Join(s.root, "subtasks", st.ID+".json")
	if err := writeJSON(path, st); err != nil {
		fmt.Printf("kanban: write subtask: %v\n", err)
	}
	return st
}

// ToggleSubTask flips the completed state of a subtask.
func (s *Store) ToggleSubTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "subtasks", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st SubTask
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	st.Completed = !st.Completed
	return writeJSON(path, st)
}

// ListSubTasks returns all subtasks for a given task ID.
func (s *Store) ListSubTasks(taskID string) ([]SubTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.unlockedListSubTasks()
	if err != nil {
		return nil, err
	}
	subtasks := make([]SubTask, 0, len(all))
	for _, st := range all {
		if st.TaskID == taskID {
			subtasks = append(subtasks, st)
		}
	}
	return subtasks, nil
}

// unlockedListSubTasks loads all subtasks without locking (caller must hold s.mu).
func (s *Store) unlockedListSubTasks() ([]SubTask, error) {
	ents, err := os.ReadDir(filepath.Join(s.root, "subtasks"))
	if err != nil {
		return nil, err
	}
	subtasks := make([]SubTask, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			b, err := os.ReadFile(filepath.Join(s.root, "subtasks", e.Name()))
			if err != nil {
				continue
			}
			var st SubTask
			if err := json.Unmarshal(b, &st); err != nil {
				continue
			}
			subtasks = append(subtasks, st)
		}
	}
	return subtasks, nil
}

// unlockedGetEpic loads an epic without locking (caller must hold s.mu).
func (s *Store) unlockedGetEpic(id string) (Epic, error) {
	var ep Epic
	b, err := os.ReadFile(filepath.Join(s.root, "epics", id+".json"))
	if err != nil {
		return ep, err
	}
	return ep, json.Unmarshal(b, &ep)
}
