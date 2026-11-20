package reviewstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestStoreRoundTrip saves a record with every field set, loads it back,
// and checks that every field survived.
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Record{
		PR:            268,
		Branch:        "phase-pr-review-queues",
		Head:          "abc123",
		AuthorModel:   "qwen3.8-27b",
		ReviewerModel: "sonnet",
		Token:         "sonnet",
		AuthorStamp:   "abc123",
		ReviewerStamp: "",
		Rounds:        1,
		OpenPoints:    2,
		PlaneItem:     "REV-4",
		Held:          true,
		TurnPushed:    false,
	}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file exists and is 0600.
	fi, err := os.Stat(filepath.Join(dir, "pr-268.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}

	// Record has only string, int and bool fields, so == compares every
	// field.
	got, err := Load(dir, 268)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != in {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, in)
	}

	// A missing file is a zero record plus os.ErrNotExist.
	zero, err := Load(dir, 999)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load missing: err = %v, want os.ErrNotExist", err)
	}
	if zero != (Record{}) {
		t.Errorf("Load missing: record = %+v, want zero", zero)
	}
}
