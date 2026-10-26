package kanban

import (
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
