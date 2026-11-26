package agentjob

import (
	"errors"
	"testing"
)

// TestCheckpointSetsSchema: a checkpoint stamps the state it writes with
// this runtime's loop-file schema version (ELR M1.2f), so a newer
// runtime can tell which layout the job's loop files have.
func TestCheckpointSetsSchema(t *testing.T) {
	s := newRunningJob(t)
	if err := s.Checkpoint("j1", toolCallCheckpoint()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.State.LoopSchema != 1 {
		t.Errorf("LoopSchema = %d, want 1", j.State.LoopSchema)
	}
}

// TestCheckpointRefusesNewer: a job whose loop files were written by a
// newer runtime (LoopSchema 2) is refused before anything is written —
// the message log stays empty.
func TestCheckpointRefusesNewer(t *testing.T) {
	s := newRunningJob(t)
	if err := s.SetStatus("j1", State{Status: Running, LoopSchema: 2}); err != nil {
		t.Fatal(err)
	}
	err := s.Checkpoint("j1", toolCallCheckpoint())
	if !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Checkpoint err = %v, want ErrNewerSchema", err)
	}
	msgs, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("Messages has %d entries, want 0", len(msgs))
	}
}

// TestRecoverLoopRefusesNewer: the recovery path refuses a newer schema
// too — it must not rebuild ops into files it does not understand.
func TestRecoverLoopRefusesNewer(t *testing.T) {
	s := newRunningJob(t)
	if err := s.SetStatus("j1", State{Status: Running, LoopSchema: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverLoop("j1"); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("RecoverLoop err = %v, want ErrNewerSchema", err)
	}
}

// TestSchemaZeroAccepted: a job with LoopSchema 0 (no loop files yet,
// or written before the field existed) is always accepted.
func TestSchemaZeroAccepted(t *testing.T) {
	s := newRunningJob(t)
	if err := s.SetStatus("j1", State{Status: Running}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint("j1", toolCallCheckpoint()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("RecoverLoop: %v", err)
	}
}
