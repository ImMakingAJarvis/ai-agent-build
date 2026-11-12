package kanban

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateTaskFields(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("epic", "")
	tk := s.AddTask(ep.ID, "task", "", 3)

	if err := s.UpdateTaskTitle(tk.ID, "new title"); err != nil {
		t.Fatalf("UpdateTaskTitle: %v", err)
	}
	if err := s.UpdateTaskPriority(tk.ID, 8); err != nil {
		t.Fatalf("UpdateTaskPriority: %v", err)
	}
	if err := s.UpdateTaskModel(tk.ID, "jarvis-brain"); err != nil {
		t.Fatalf("UpdateTaskModel: %v", err)
	}

	got, err := s.GetTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "new title" || got.Priority != 8 || got.Model != "jarvis-brain" {
		t.Errorf("task = %+v, want updated title/priority/model", got)
	}

	if err := s.UpdateEpicTitle(ep.ID, "new epic"); err != nil {
		t.Fatalf("UpdateEpicTitle: %v", err)
	}
	gep, err := s.GetEpic(ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gep.Title != "new epic" {
		t.Errorf("epic title = %q", gep.Title)
	}
}

func TestUpdateTaskPriorityZeroDefaults(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("e", "")
	tk := s.AddTask(ep.ID, "t", "", 0)
	if err := s.UpdateTaskPriority(tk.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(tk.ID)
	if got.Priority != 5 {
		t.Errorf("priority = %d, want 5", got.Priority)
	}
}

func TestDeleteTaskUnlinksEpic(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("e", "")
	a := s.AddTask(ep.ID, "a", "", 0)
	b := s.AddTask(ep.ID, "b", "", 0)
	_ = s.AddSubTask(a.ID, "sub")

	if err := s.DeleteTask(a.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.GetTask(a.ID); err == nil {
		t.Error("task a should be gone")
	}
	if _, err := s.GetTask(b.ID); err != nil {
		t.Errorf("task b should survive: %v", err)
	}
	if sts, _ := s.ListSubTasks(a.ID); len(sts) != 0 {
		t.Errorf("subtasks of deleted task should be gone, got %v", sts)
	}
	if all, _ := s.ListSubTasks(b.ID); len(all) != 0 {
		t.Errorf("other task's subtasks should survive, got %v", all)
	}
	gep, err := s.GetEpic(ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gep.TaskIDs) != 1 || gep.TaskIDs[0] != b.ID {
		t.Errorf("epic TaskIDs = %v, want only b", gep.TaskIDs)
	}
}

func TestDeleteEpicCascades(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("e", "")
	tk := s.AddTask(ep.ID, "t", "", 0)

	if err := s.DeleteEpic(ep.ID); err != nil {
		t.Fatalf("DeleteEpic: %v", err)
	}
	if _, err := s.GetEpic(ep.ID); err == nil {
		t.Error("epic should be gone")
	}
	if _, err := s.GetTask(tk.ID); err == nil {
		t.Error("task should be gone")
	}
	if tasks, _ := s.ListTasks(); len(tasks) != 0 {
		t.Errorf("ListTasks = %v, want empty", tasks)
	}
}

// TestEmptyListsMarshalAsArrays locks the API contract: empty lists must
// serialize as [] (not null), because the dashboard page iterates them in JS
// and a null response crashed the board render.
func TestEmptyListsMarshalAsArrays(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epics, err := s.ListEpics()
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	subtasks, err := s.ListSubTasks("missing-task")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		v    any
	}{
		{"epics", epics},
		{"tasks", tasks},
		{"subtasks", subtasks},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "[]" {
			t.Errorf("%s = %s, want []", c.name, b)
		}
	}
}

func TestDeleteMissing(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEpic("nope"); err == nil {
		t.Error("DeleteEpic of missing epic should error")
	}
	if err := s.DeleteTask("nope"); err == nil {
		t.Error("DeleteTask of missing task should error")
	}
}

// TestUpdateTaskNightlyRoundTrip covers NoBrakes 4/6: a task can be tagged
// nightly and the tag is persisted to its JSON, and the tag can be cleared.
func TestUpdateTaskNightlyRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("epic", "")
	tk := s.AddTask(ep.ID, "task", "", 5)

	// A fresh task is not nightly, and the field is omitted from its JSON.
	raw, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); strings.Contains(got, "nightly") {
		t.Errorf("fresh task JSON carries nightly: %s", got)
	}

	if err := s.UpdateTaskNightly(tk.ID, true); err != nil {
		t.Fatalf("UpdateTaskNightly(true): %v", err)
	}
	got, err := s.GetTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Nightly {
		t.Errorf("task.Nightly = false after set, want true")
	}
	// The tag survives a reload from disk (it is stored on the task JSON).
	if raw, _ := os.ReadFile(filepath.Join(s.root, "tasks", tk.ID+".json")); !strings.Contains(string(raw), `"nightly": true`) {
		t.Errorf("task JSON does not persist nightly: %s", raw)
	}

	// Clearing the tag works too.
	if err := s.UpdateTaskNightly(tk.ID, false); err != nil {
		t.Fatalf("UpdateTaskNightly(false): %v", err)
	}
	if got, _ := s.GetTask(tk.ID); got.Nightly {
		t.Errorf("task.Nightly = true after clear, want false")
	}
}

// TestListNightlyTasks is the queryable nightly set (NoBrakes 4/6): only
// tagged tasks are returned, in ListTasks order; an empty set marshals as [].
func TestListNightlyTasks(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("epic", "")
	a := s.AddTask(ep.ID, "a", "", 1)
	b := s.AddTask(ep.ID, "b", "", 9)
	s.AddTask(ep.ID, "c", "", 5) // left untagged; must not appear
	// Tag the lowest and the highest priority task, not the middle one.
	if err := s.UpdateTaskNightly(a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskNightly(b.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListNightlyTasks()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, t := range got {
		ids = append(ids, t.ID)
	}
	// ListTasks order is priority desc, so b (9) precedes a (1); c is absent.
	if len(ids) != 2 || ids[0] != b.ID || ids[1] != a.ID {
		t.Fatalf("ListNightlyTasks = %v, want [b a]", ids)
	}

	// With nothing tagged the set is empty but non-nil (marshals as []).
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep2 := s2.AddEpic("epic2", "")
	s2.AddTask(ep2.ID, "plain", "", 5)
	empty, err := s2.ListNightlyTasks()
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil {
		t.Fatal("ListNightlyTasks on an empty board = nil, want non-nil empty")
	}
	if b, _ := json.Marshal(empty); string(b) != "[]" {
		t.Errorf("empty nightly set marshals as %s, want []", b)
	}
}

// TestCommentRoundTrip covers the comment lifecycle: add onto an existing
// task, list oldest-first, delete, and the error paths (missing task,
// missing comment).
func TestCommentRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("epic", "")
	tk := s.AddTask(ep.ID, "task", "", 5)

	// A comment cannot orphan onto a missing task.
	if _, err := s.AddComment("no-such-task", "agent", "hi"); err == nil {
		t.Error("AddComment on missing task should error")
	}

	c1, err := s.AddComment(tk.ID, "nathan", "first")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if _, err := s.AddComment(tk.ID, "agent", "second"); err != nil {
		t.Fatalf("AddComment second: %v", err)
	}

	// Persisted as <id>.json under comments/ with the fields intact.
	raw, err := os.ReadFile(filepath.Join(s.root, "comments", c1.ID+".json"))
	if err != nil {
		t.Fatalf("comment file missing: %v", err)
	}
	var onDisk Comment
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.TaskID != tk.ID || onDisk.Author != "nathan" || onDisk.Body != "first" {
		t.Errorf("comment on disk = %+v", onDisk)
	}

	// Oldest first, both present.
	list, err := s.ListComments(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != c1.ID || list[1].Body != "second" {
		t.Errorf("ListComments = %+v, want [first second] oldest first", list)
	}

	// Another task's comments stay out.
	other := s.AddTask(ep.ID, "other", "", 5)
	if got, _ := s.ListComments(other.ID); len(got) != 0 {
		t.Errorf("other task comments = %v, want empty", got)
	}

	// Delete works; deleting again errors (no silent no-op).
	if err := s.DeleteComment(c1.ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if _, err := s.GetComment(c1.ID); err == nil {
		t.Error("comment should be gone")
	}
	if err := s.DeleteComment(c1.ID); err == nil {
		t.Error("DeleteComment of missing comment should error")
	}
	if got, _ := s.ListComments(tk.ID); len(got) != 1 || got[0].Body != "second" {
		t.Errorf("after delete ListComments = %+v", got)
	}
}

// TestCommentListEmptyMarshalsAsArray keeps the API contract: an empty
// comment list is [] and never null, matching the other list endpoints.
func TestCommentListEmptyMarshalsAsArray(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	comments, err := s.ListComments("missing-task")
	if err != nil {
		t.Fatal(err)
	}
	if comments == nil {
		t.Fatal("ListComments on a missing task = nil, want non-nil empty")
	}
	if b, _ := json.Marshal(comments); string(b) != "[]" {
		t.Errorf("empty comment list marshals as %s, want []", b)
	}
}

// TestDeleteTaskRemovesComments is the cascade: deleting a task takes its
// comments with it, and other tasks' comments survive.
func TestDeleteTaskRemovesComments(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("e", "")
	a := s.AddTask(ep.ID, "a", "", 0)
	b := s.AddTask(ep.ID, "b", "", 0)
	ca, err := s.AddComment(a.ID, "agent", "on a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(b.ID, "agent", "on b"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTask(a.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.GetComment(ca.ID); err == nil {
		t.Error("comment of deleted task should be gone")
	}
	if got, _ := s.ListComments(b.ID); len(got) != 1 {
		t.Errorf("surviving task's comments = %v, want 1", got)
	}
}

// TestDeleteEpicRemovesComments is the epic-level cascade.
func TestDeleteEpicRemovesComments(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := s.AddEpic("e", "")
	tk := s.AddTask(ep.ID, "t", "", 0)
	c, err := s.AddComment(tk.ID, "nathan", "q")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEpic(ep.ID); err != nil {
		t.Fatalf("DeleteEpic: %v", err)
	}
	if _, err := s.GetComment(c.ID); err == nil {
		t.Error("comment should be gone with its epic")
	}
}
