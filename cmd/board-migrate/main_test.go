package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

func writeBoard(t *testing.T, epics []kanban.Epic, tasks []kanban.Task) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "jarvis")
	if err := os.MkdirAll(filepath.Join(root, "kanban", "epics"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "kanban", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range epics {
		b, _ := json.Marshal(e)
		if err := os.WriteFile(filepath.Join(root, "kanban", "epics", e.ID+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tk := range tasks {
		b, _ := json.Marshal(tk)
		if err := os.WriteFile(filepath.Join(root, "kanban", "tasks", tk.ID+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func at(s string) time.Time {
	tk, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tk
}

func TestLoadBoardFiltersDoneAndKeepsOrder(t *testing.T) {
	epics := []kanban.Epic{
		{ID: "eeeeeeee-0000-4000-8000-000000000002", Title: "Later epic", TaskIDs: []string{"22222222-0000-4000-8000-000000000002"}, CreatedAt: at("2026-10-01T00:00:00Z")},
		{ID: "eeeeeeee-0000-4000-8000-000000000001", Title: "First epic",
			TaskIDs: []string{"11111111-0000-4000-8000-000000000002", "11111111-0000-4000-8000-000000000001", "11111111-0000-4000-8000-000000000003"},
			CreatedAt: at("2026-10-01T00:00:00Z")},
		{ID: "eeeeeeee-0000-4000-8000-000000000003", Title: "Lone epic", TaskIDs: nil, CreatedAt: at("2026-10-01T00:00:00Z")},
	}
	tasks := []kanban.Task{
		{ID: "11111111-0000-4000-8000-000000000001", EpicID: "eeeeeeee-0000-4000-8000-000000000001", Title: "t1", Status: kanban.StatusBacklog, CreatedAt: at("2026-10-01T00:01:00Z")},
		{ID: "11111111-0000-4000-8000-000000000002", EpicID: "eeeeeeee-0000-4000-8000-000000000001", Title: "t2", Status: kanban.StatusDone, CreatedAt: at("2026-10-01T00:02:00Z")},
		{ID: "11111111-0000-4000-8000-000000000003", EpicID: "eeeeeeee-0000-4000-8000-000000000001", Title: "t3", Status: kanban.StatusReview, CreatedAt: at("2026-10-01T00:03:00Z")},
		{ID: "22222222-0000-4000-8000-000000000002", EpicID: "eeeeeeee-0000-4000-8000-000000000002", Title: "t4", Status: kanban.StatusInProgress, CreatedAt: at("2026-10-01T00:04:00Z")},
	}
	b, err := loadBoard(writeBoard(t, epics, tasks))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Epics) != 3 {
		t.Fatalf("epics = %d, want 3 (every epic is open)", len(b.Epics))
	}
	// Epic order: by creation time, then ID (all equal here).
	if b.Epics[0].ID != "eeeeeeee-0000-4000-8000-000000000001" || b.Epics[2].ID != "eeeeeeee-0000-4000-8000-000000000003" {
		t.Fatalf("epic order wrong: %v", boardEpicIDs(b.Epics))
	}
	// Open tasks only: done (t2) dropped.
	if len(b.Tasks) != 3 {
		t.Fatalf("open tasks = %d, want 3 (done filtered out)", len(b.Tasks))
	}
	got := make([]string, len(b.Tasks))
	for i, tk := range b.Tasks {
		got[i] = tk.Title
	}
	// First epic's tasks in task_ids order (t2 filtered), then the later epic's.
	want := []string{"t1", "t3", "t4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("task order = %v, want %v", got, want)
		}
	}
	if b.TotalTasks != 4 {
		t.Fatalf("TotalTasks = %d, want 4", b.TotalTasks)
	}
}

func TestStateNameFor(t *testing.T) {
	cases := map[kanban.Status]string{
		kanban.StatusBacklog:    "Backlog",
		kanban.StatusInProgress: "In Progress",
		kanban.StatusReview:     "In Progress",
		kanban.StatusDone:       "Backlog", // never migrated; defensive default
	}
	for in, want := range cases {
		if got := stateNameFor(in); got != want {
			t.Errorf("stateNameFor(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestDescriptionsCarryFindableMarkers(t *testing.T) {
	e := kanban.Epic{ID: "eeeeeeee-0000-4000-8000-000000000001", Title: "E", Description: "a <b> c & d"}
	tk := kanban.Task{ID: "11111111-0000-4000-8000-000000000001", EpicID: "eeeeeeee-0000-4000-8000-000000000001", Title: "T", Content: "do <x>", Status: kanban.StatusReview, Priority: 7, Model: "local"}

	ed, td := epicDescription(e), taskDescription(tk)
	if !strings.Contains(ed, "a &lt;b&gt; c &amp; d") {
		t.Errorf("epic description not escaped: %s", ed)
	}
	items := []plane.WorkItem{
		{ID: "plane-epic-1", DescriptionHTML: ed},
		{ID: "plane-task-1", DescriptionHTML: td},
		{ID: "plane-unrelated", DescriptionHTML: "<p>no marker here</p>"},
	}
	haveEpics, haveTasks := indexItems(items)
	if haveEpics[e.ID] != "plane-epic-1" {
		t.Errorf("epic marker not found: %v", haveEpics)
	}
	if haveTasks[tk.ID] != "plane-task-1" {
		t.Errorf("task marker not found: %v", haveTasks)
	}
	// The task line also contains "epic=<uuid>" — it must not be picked up
	// by the epic regex (which requires "epic=" right after "old-board ").
	if _, ok := haveEpics[tk.ID]; ok {
		t.Errorf("task marker leaked into epic index: %v", haveEpics)
	}
	if _, ok := haveEpics["plane-task-1"]; ok {
		t.Errorf("task item indexed as epic: %v", haveEpics)
	}
	if len(haveEpics) != 1 || len(haveTasks) != 1 {
		t.Errorf("extra markers: epics=%v tasks=%v", haveEpics, haveTasks)
	}
	if !strings.Contains(td, "old status: review; priority: 7; old model: local") {
		t.Errorf("task metadata missing: %s", td)
	}
}

func TestMissing(t *testing.T) {
	have := map[string]string{"a": "1", "b": "2"}
	got := missing([]string{"a", "b", "c"}, have)
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("missing = %v, want [c]", got)
	}
	if got := missing([]string{}, have); len(got) != 0 {
		t.Fatalf("missing(empty) = %v", got)
	}
}
