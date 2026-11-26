package agentjob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoopFieldsRoundTrip (ELR M1.2a): a job ingested with the new runtime
// Spec fields and the new State fields survives a write/read round trip —
// Get returns them unchanged.
func TestLoopFieldsRoundTrip(t *testing.T) {
	s := newStore(t)
	spec := Spec{
		ID:         "lf1",
		Title:      "loop fields",
		Instructions: "round trip the runtime fields",
		CreatedAt:  time.Now().UTC(),
		ParentID:   "root-job",
		RootID:     "root-job",
		Capability: "image.generate",
		WantModel:  "qwen3.8-27b",
		Workspace:  "/work/lf1",
		Acceptance: []string{"tests pass", "docs updated"},
	}
	if added, err := s.Ingest(spec); err != nil || !added {
		t.Fatalf("ingest: added=%v err=%v", added, err)
	}
	slot := 2
	st := State{
		Status:     Waiting,
		WaitReason: WaitModel,
		Tokens:     4096,
		WarmSlot:   &slot,
		Snapshot: &SnapshotRef{
			File:        "/snap/lf1.md",
			Fingerprint: "fp-123",
			Tokens:      4096,
			Bytes:       20480,
			Tier:        "hot",
		},
	}
	if err := s.SetStatus("lf1", st); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("lf1")
	if err != nil {
		t.Fatal(err)
	}
	// Spec fields.
	if j.ParentID != "root-job" || j.RootID != "root-job" {
		t.Errorf("parent_id/root_id = %q/%q, want root-job/root-job", j.ParentID, j.RootID)
	}
	if j.Capability != "image.generate" {
		t.Errorf("capability = %q, want image.generate", j.Capability)
	}
	if j.WantModel != "qwen3.8-27b" {
		t.Errorf("want_model = %q, want qwen3.8-27b", j.WantModel)
	}
	if j.Workspace != "/work/lf1" {
		t.Errorf("workspace = %q, want /work/lf1", j.Workspace)
	}
	if len(j.Acceptance) != 2 || j.Acceptance[0] != "tests pass" || j.Acceptance[1] != "docs updated" {
		t.Errorf("acceptance = %v, want [tests pass docs updated]", j.Acceptance)
	}
	// State fields.
	if j.WaitReason != WaitModel {
		t.Errorf("wait_reason = %q, want %q", j.WaitReason, WaitModel)
	}
	if j.Tokens != 4096 {
		t.Errorf("tokens = %d, want 4096", j.Tokens)
	}
	if j.WarmSlot == nil || *j.WarmSlot != 2 {
		t.Errorf("warm_slot = %v, want 2", j.WarmSlot)
	}
	if j.Snapshot == nil || j.Snapshot.File != "/snap/lf1.md" || j.Snapshot.Fingerprint != "fp-123" ||
		j.Snapshot.Tokens != 4096 || j.Snapshot.Bytes != 20480 || j.Snapshot.Tier != "hot" {
		t.Errorf("snapshot = %+v, want the ingested ref", j.Snapshot)
	}
}

// TestLoopFieldsOldJobLoads (ELR M1.2a): a job directory written by hand
// with only the old fields (the JSON shape of TestStoreMigrationOldFormat)
// still loads, and every new field is zero.
func TestLoopFieldsOldJobLoads(t *testing.T) {
	s := newStore(t)
	oldSpec := `{"id":"lf-old","title":"old job","instructions":"do it","priority":7,"created_at":"2026-10-01T00:00:00Z","class":"build"}`
	oldState := `{"status":"ready","ran_with":"local"}`
	dir := s.JobDir("lf-old")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(oldSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldState), 0o644); err != nil {
		t.Fatal(err)
	}

	j, err := s.Get("lf-old")
	if err != nil {
		t.Fatalf("get old job: %v", err)
	}
	if j.Status != Ready || j.Priority != 7 {
		t.Errorf("old job = %+v, want ready/7", j)
	}
	// The new Spec fields are zero.
	if j.ParentID != "" || j.RootID != "" || j.Capability != "" ||
		j.WantModel != "" || j.Workspace != "" || j.Acceptance != nil {
		t.Errorf("new spec fields not zero: parent=%q root=%q cap=%q want=%q ws=%q acc=%v",
			j.ParentID, j.RootID, j.Capability, j.WantModel, j.Workspace, j.Acceptance)
	}
	// The new State fields are zero.
	if j.WaitReason != "" || j.Tokens != 0 || j.WarmSlot != nil || j.Snapshot != nil {
		t.Errorf("new state fields not zero: wait_reason=%q tokens=%d warm_slot=%v snapshot=%+v",
			j.WaitReason, j.Tokens, j.WarmSlot, j.Snapshot)
	}
}
