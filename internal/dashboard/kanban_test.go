package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
)

// kanbanDo sends a request through the server's real route table.
func kanbanDo(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func kanbanGet(t *testing.T, s *Server, path string) []byte {
	t.Helper()
	w := kanbanDo(t, s, "GET", path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// TestKanbanEmptyBoardReturnsArrays is the regression test for the bug where
// an empty board made the list endpoints answer "null" (Go nil slice). The
// board page iterates the arrays in JS, so null threw "not iterable" and the
// board rendered empty even though epics had been saved.
func TestKanbanEmptyBoardReturnsArrays(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{
		"/api/kanban/epics",
		"/api/kanban/tasks",
		"/api/kanban/subtasks?task_id=missing",
		"/api/kanban/tasks?epic_id=missing",
	} {
		body := kanbanGet(t, s, path)
		if strings.TrimSpace(string(body)) != "[]" {
			t.Errorf("GET %s = %q, want []", path, strings.TrimSpace(string(body)))
		}
	}
}

func TestKanbanEpicTaskLifecycle(t *testing.T) {
	s := newTestServer(t)

	// Create epic.
	w := kanbanDo(t, s, "POST", "/api/kanban/epics", `{"title":"E1","description":"d"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create epic: %d %s", w.Code, w.Body.String())
	}
	var ep kanban.Epic
	if err := json.Unmarshal(w.Body.Bytes(), &ep); err != nil {
		t.Fatal(err)
	}
	if ep.ID == "" {
		t.Fatal("epic has no id")
	}

	// Epic is listed.
	if got := kanbanGet(t, s, "/api/kanban/epics"); !strings.Contains(string(got), "E1") {
		t.Fatalf("epic list does not contain E1: %s", got)
	}

	// Create task in the epic.
	w = kanbanDo(t, s, "POST", "/api/kanban/tasks",
		`{"epic_id":"`+ep.ID+`","title":"T1","content":"c","priority":7,"model":"sonnet"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create task: %d %s", w.Code, w.Body.String())
	}
	var tk kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tk); err != nil {
		t.Fatal(err)
	}
	if tk.Status != kanban.StatusBacklog || tk.Priority != 7 || tk.Model != "sonnet" {
		t.Fatalf("task = %+v, want backlog/p7/sonnet", tk)
	}

	// Task listed, unfiltered and epic-filtered.
	if got := kanbanGet(t, s, "/api/kanban/tasks"); !strings.Contains(string(got), "T1") {
		t.Fatalf("task list missing T1: %s", got)
	}
	if got := kanbanGet(t, s, "/api/kanban/tasks?epic_id="+ep.ID); !strings.Contains(string(got), "T1") {
		t.Fatalf("filtered task list missing T1: %s", got)
	}

	// Subtasks of a task with none: [] (the other half of the null bug).
	if got := kanbanGet(t, s, "/api/kanban/subtasks?task_id="+tk.ID); strings.TrimSpace(string(got)) != "[]" {
		t.Fatalf("empty subtask list = %q, want []", got)
	}

	// Add a subtask, then toggle it.
	w = kanbanDo(t, s, "POST", "/api/kanban/subtasks",
		`{"task_id":"`+tk.ID+`","title":"S1"}`)
	var st kanban.SubTask
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	w = kanbanDo(t, s, "PATCH", "/api/kanban/subtasks/"+st.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("toggle subtask: %d", w.Code)
	}
	got, _ := s.kanban.store.ListSubTasks(tk.ID)
	if len(got) != 1 || !got[0].Completed {
		t.Fatalf("subtasks after toggle = %+v, want one completed", got)
	}

	// Move the task to review.
	w = kanbanDo(t, s, "PATCH", "/api/kanban/tasks/"+tk.ID, `{"status":"review"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update task: %d %s", w.Code, w.Body.String())
	}
	gotTask, _ := s.kanban.store.GetTask(tk.ID)
	if gotTask.Status != kanban.StatusReview {
		t.Fatalf("task status = %s, want review", gotTask.Status)
	}

	// Delete the task: gone from the board and unlinked from the epic.
	w = kanbanDo(t, s, "DELETE", "/api/kanban/tasks/"+tk.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete task: %d", w.Code)
	}
	if got := kanbanGet(t, s, "/api/kanban/tasks?epic_id="+ep.ID); strings.TrimSpace(string(got)) != "[]" {
		t.Fatalf("tasks after delete = %s, want []", got)
	}
	gep, err := s.kanban.store.GetEpic(ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gep.TaskIDs) != 0 {
		t.Fatalf("epic TaskIDs = %v, want empty", gep.TaskIDs)
	}

	// Delete the epic.
	w = kanbanDo(t, s, "DELETE", "/api/kanban/epics/"+ep.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete epic: %d", w.Code)
	}
	if got := kanbanGet(t, s, "/api/kanban/epics"); strings.TrimSpace(string(got)) != "[]" {
		t.Fatalf("epics after delete = %s, want []", got)
	}
}
