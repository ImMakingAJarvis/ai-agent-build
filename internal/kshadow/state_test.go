package kshadow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")

	s := NewState()
	q := s.Queue("voice")
	q.Exchange = "tasks"
	q.Seq = 7
	q.Written = 6
	q.Mismatch = 1
	q.LastID = "job-1"
	q.LastAt = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	q.BaselinePub = 1000
	q.BaselineDeliver = 999
	q.WatermarkSeq = 5
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.StartedAt.IsZero() || !got.StartedAt.Equal(s.StartedAt) {
		t.Errorf("started_at: %v", got.StartedAt)
	}
	gq := got.Queues["voice"]
	if gq == nil {
		t.Fatal("voice queue missing")
	}
	if gq.Seq != 7 || gq.Written != 6 || gq.Mismatch != 1 || gq.LastID != "job-1" ||
		gq.BaselinePub != 1000 || gq.BaselineDeliver != 999 || gq.WatermarkSeq != 5 {
		t.Errorf("counters: %+v", gq)
	}
	if !gq.LastAt.Equal(q.LastAt) {
		t.Errorf("last_at: %v", gq.LastAt)
	}
	// no temp file left behind
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind")
	}
}

func TestLoadStateMissing(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must be fresh, not an error: %v", err)
	}
	if s.Queues == nil || s.StartedAt.IsZero() {
		t.Errorf("fresh state: %+v", s)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Error("corrupt file must be an error")
	}
}
