package agentjob

import (
	"strings"
	"testing"
	"time"
)

// TestRefresh covers B5's repeated re-plan: a finished job is re-triggered
// in place with new instructions, while an in-flight or missing job is
// refused.
func TestRefresh(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		ID:           "j1",
		Title:        "re-plan",
		Instructions: "first instructions",
		Class:        ClassBackground,
		Priority:     100,
		CreatedAt:    time.Now().UTC(),
	}
	if added, err := s.Ingest(spec); err != nil || !added {
		t.Fatalf("ingest: added=%v err=%v", added, err)
	}

	// A fresh (ready) job is not finished: Refresh must refuse.
	if err := s.Refresh(spec); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("refresh ready job: err=%v, want not-finished refusal", err)
	}

	// Same for a claimed job.
	if _, err := s.Claim("j1", "r1", "driver"); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(spec); err == nil {
		t.Fatal("refresh claimed job: want not-finished refusal")
	}

	// A done job is re-triggered: new instructions, back to ready.
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	j.State.Status = Done
	j.State.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.SetStatus("j1", j.State); err != nil {
		t.Fatal(err)
	}
	spec.Instructions = "second instructions"
	if err := s.Refresh(spec); err != nil {
		t.Fatalf("refresh done job: %v", err)
	}
	j, err = s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Ready {
		t.Errorf("status = %q, want %q", j.Status, Ready)
	}
	if j.Instructions != "second instructions" {
		t.Errorf("instructions = %q, want the refreshed ones", j.Instructions)
	}

	// A missing job is an error, not a silent ingest.
	if err := s.Refresh(Spec{ID: "nope", CreatedAt: time.Now()}); err == nil {
		t.Fatal("refresh missing job: want error")
	}
}

// TestEdit covers editing the text of a queued (ready) job: the title and
// instructions are replaced field-by-field, non-ready jobs and a missing
// job are refused, and a no-op edit is an error.
func TestEdit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		ID:           "e1",
		Title:        "original title",
		Instructions: "original instructions",
		Class:        ClassBuild,
		Priority:     5,
		CreatedAt:    time.Now().UTC(),
	}
	if added, err := s.Ingest(spec); err != nil || !added {
		t.Fatalf("ingest: added=%v err=%v", added, err)
	}

	// Editing only the title leaves the instructions alone.
	if err := s.Edit("e1", "new title", ""); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	j, err := s.Get("e1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "new title" {
		t.Errorf("title = %q, want new title", j.Title)
	}
	if j.Instructions != "original instructions" {
		t.Errorf("instructions = %q, want unchanged", j.Instructions)
	}

	// Editing only the instructions leaves the title and everything else
	// (class, priority, state) alone.
	if err := s.Edit("e1", "", "new instructions"); err != nil {
		t.Fatalf("edit instructions: %v", err)
	}
	j, err = s.Get("e1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "new title" {
		t.Errorf("title = %q, want unchanged", j.Title)
	}
	if j.Instructions != "new instructions" {
		t.Errorf("instructions = %q, want new instructions", j.Instructions)
	}
	if j.Class != ClassBuild || j.Priority != 5 || j.Status != Ready {
		t.Errorf("class/priority/status = %s/%d/%s, want build/5/%s", j.Class, j.Priority, j.Status, Ready)
	}

	// Nothing given: an error, no write.
	if err := s.Edit("e1", "", ""); err == nil {
		t.Fatal("empty edit: want error")
	}
	// A not-ready job is refused.
	if _, err := s.Claim("e1", "r1", "driver"); err != nil {
		t.Fatal(err)
	}
	if err := s.Edit("e1", "x", "y"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("edit claimed job: err=%v, want not-ready refusal", err)
	}
	// A missing job is an error.
	if err := s.Edit("nope", "x", "y"); err == nil {
		t.Fatal("edit missing job: want error")
	}
}
