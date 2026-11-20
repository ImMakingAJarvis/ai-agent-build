package reviewstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRevival covers an append, a second append, and the Los Angeles day
// split: 2026-11-20T07:30:00Z is 23:30 on 2026-11-19 in Los Angeles
// (standard time, UTC-8).
func TestRevival(t *testing.T) {
	dir := t.TempDir()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// The 07:30 UTC stamp is the previous evening in Los Angeles.
	evening := time.Date(2026, 11, 20, 7, 30, 0, 0, time.UTC)
	// 20:00 UTC is noon the same day in Los Angeles.
	noon := time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC)

	first := Revival{At: evening, PR: 268, State: "waiting", Enqueued: "review.sonnet"}
	if err := AppendRevival(dir, first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The log file is 0600.
	fi, err := os.Stat(filepath.Join(dir, "revivals.jsonl"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}

	second := Revival{At: noon, PR: 269, State: "waiting", Enqueued: "review.local"}
	if err := AppendRevival(dir, second); err != nil {
		t.Fatalf("second append: %v", err)
	}

	// Both appends are in the log.
	all, err := RevivalsOn(dir, noon) // 2026-11-20 LA day
	if err != nil {
		t.Fatalf("RevivalsOn: %v", err)
	}
	if len(all) != 1 || all[0] != second {
		t.Errorf("2026-11-20 LA day = %+v, want only the noon line %+v", all, second)
	}

	// The 07:30 UTC line is on 2026-11-19 in Los Angeles, not 2026-11-20.
	got19, err := RevivalsOn(dir, time.Date(2026, 11, 19, 12, 0, 0, 0, la))
	if err != nil {
		t.Fatalf("RevivalsOn 11-19: %v", err)
	}
	if len(got19) != 1 || got19[0] != first {
		t.Errorf("2026-11-19 LA day = %+v, want the 07:30 UTC line %+v", got19, first)
	}

	// A missing log is an empty result, not an error.
	none, err := RevivalsOn(t.TempDir(), noon)
	if err != nil {
		t.Fatalf("RevivalsOn on empty dir: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("empty dir = %+v, want empty", none)
	}
}
