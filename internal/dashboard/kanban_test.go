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

// TestKanbanNightlyTagAPISet covers NoBrakes 4/6 acceptance criterion 1:
// a task can be tagged nightly via the API (on create and on update) and
// the tag round-trips through GET; the ?nightly=true filter returns the
// auto-eligible set.
func TestKanbanNightlyTagAPISet(t *testing.T) {
	s := newTestServer(t)

	w := kanbanDo(t, s, "POST", "/api/kanban/epics", `{"title":"E1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create epic: %d %s", w.Code, w.Body.String())
	}
	var ep kanban.Epic
	if err := json.Unmarshal(w.Body.Bytes(), &ep); err != nil {
		t.Fatal(err)
	}

	// One task tagged nightly at creation, one left plain.
	w = kanbanDo(t, s, "POST", "/api/kanban/tasks",
		`{"epic_id":"`+ep.ID+`","title":"tagged","priority":9,"nightly":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create tagged task: %d %s", w.Code, w.Body.String())
	}
	var tagged kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tagged); err != nil {
		t.Fatal(err)
	}
	if !tagged.Nightly {
		t.Fatalf("created task = %+v, want nightly=true", tagged)
	}
	w = kanbanDo(t, s, "POST", "/api/kanban/tasks",
		`{"epic_id":"`+ep.ID+`","title":"plain","priority":5}`)
	var plain kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Nightly {
		t.Fatalf("created task = %+v, want nightly unset", plain)
	}

	// The nightly filter returns exactly the tagged task.
	if got := kanbanGet(t, s, "/api/kanban/tasks?nightly=true"); !strings.Contains(string(got), "tagged") || strings.Contains(string(got), "plain") {
		t.Fatalf("nightly filter = %s, want only the tagged task", got)
	}

	// Tag the plain task via PATCH. Each step unmarshals into a fresh
	// struct: nightly is omitempty, so reusing a struct would keep the
	// stale value when the field is absent from the response.
	w = kanbanDo(t, s, "PATCH", "/api/kanban/tasks/"+plain.ID, `{"nightly":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch nightly=true: %d %s", w.Code, w.Body.String())
	}
	var tagged2 kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tagged2); err != nil {
		t.Fatal(err)
	}
	if !tagged2.Nightly {
		t.Fatalf("patched task = %+v, want nightly=true", tagged2)
	}

	// Clear it again with an explicit false (not just absent).
	w = kanbanDo(t, s, "PATCH", "/api/kanban/tasks/"+plain.ID, `{"nightly":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch nightly=false: %d %s", w.Code, w.Body.String())
	}
	var cleared kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Nightly {
		t.Fatalf("cleared task = %+v, want nightly=false", cleared)
	}

	// An absent field leaves the tag untouched.
	w = kanbanDo(t, s, "PATCH", "/api/kanban/tasks/"+tagged.ID, `{"status":"review"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status: %d %s", w.Code, w.Body.String())
	}
	var stillTagged kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &stillTagged); err != nil {
		t.Fatal(err)
	}
	if !stillTagged.Nightly {
		t.Fatalf("task lost nightly tag after an unrelated patch: %+v", stillTagged)
	}
}

// TestKanbanCommentRoundTrip is the API-level round trip: POST creates,
// GET lists oldest first, DELETE removes (204, then 404 on the list).
func TestKanbanCommentRoundTrip(t *testing.T) {
	s := newTestServer(t)
	w := kanbanDo(t, s, "POST", "/api/kanban/epics", `{"title":"E","description":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create epic: %d %s", w.Code, w.Body.String())
	}
	var ep kanban.Epic
	if err := json.Unmarshal(w.Body.Bytes(), &ep); err != nil {
		t.Fatal(err)
	}
	w = kanbanDo(t, s, "POST", "/api/kanban/tasks",
		`{"epic_id":"`+ep.ID+`","title":"T","content":"","priority":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create task: %d %s", w.Code, w.Body.String())
	}
	var tk kanban.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tk); err != nil {
		t.Fatal(err)
	}

	// POST onto a missing task is 404.
	w = kanbanDo(t, s, "POST", "/api/kanban/comments",
		`{"task_id":"nope","author":"nathan","body":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("comment on missing task = %d, want 404", w.Code)
	}

	// Two comments; empty list for the task is [].
	if got := kanbanGet(t, s, "/api/kanban/comments?task_id="+tk.ID); strings.TrimSpace(string(got)) != "[]" {
		t.Fatalf("empty comment list = %s, want []", got)
	}
	w = kanbanDo(t, s, "POST", "/api/kanban/comments",
		`{"task_id":"`+tk.ID+`","author":"nathan","body":"first"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create comment: %d %s", w.Code, w.Body.String())
	}
	var c1 kanban.Comment
	if err := json.Unmarshal(w.Body.Bytes(), &c1); err != nil {
		t.Fatal(err)
	}
	if c1.ID == "" || c1.TaskID != tk.ID || c1.Author != "nathan" || c1.Body != "first" {
		t.Errorf("created comment = %+v", c1)
	}
	// No author in the body -> defaults to nathan.
	w = kanbanDo(t, s, "POST", "/api/kanban/comments",
		`{"task_id":"`+tk.ID+`","body":"second"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create comment 2: %d %s", w.Code, w.Body.String())
	}
	var c2 kanban.Comment
	if err := json.Unmarshal(w.Body.Bytes(), &c2); err != nil {
		t.Fatal(err)
	}
	if c2.Author != "nathan" {
		t.Errorf("default author = %q, want nathan", c2.Author)
	}

	// GET: oldest first.
	got := kanbanGet(t, s, "/api/kanban/comments?task_id="+tk.ID)
	var list []kanban.Comment
	if err := json.Unmarshal(got, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != c1.ID || list[1].Body != "second" {
		t.Errorf("comment list = %+v, want [first second] oldest first", list)
	}

	// DELETE removes; second delete is 404.
	w = kanbanDo(t, s, "DELETE", "/api/kanban/comments/"+c1.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete comment = %d, want 204", w.Code)
	}
	w = kanbanDo(t, s, "DELETE", "/api/kanban/comments/"+c1.ID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing comment = %d, want 404", w.Code)
	}
	if got := kanbanGet(t, s, "/api/kanban/comments?task_id="+tk.ID); len(json.RawMessage(got)) > 0 {
		var after []kanban.Comment
		if err := json.Unmarshal(got, &after); err != nil {
			t.Fatal(err)
		}
		if len(after) != 1 || after[0].ID != c2.ID {
			t.Errorf("after delete list = %+v, want only second", after)
		}
	}
}
