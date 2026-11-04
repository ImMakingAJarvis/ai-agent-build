package nobrakes

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func testEntries(n int) []AuditEntry {
	out := make([]AuditEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, AuditEntry{
			Timestamp: "2026-11-03T03:00:00Z",
			Window:    "2026-11-03",
			JobID:     "job" + string(rune('a'+i)),
			Title:     "title " + string(rune('a'+i)),
			Trigger:   TriggerAutoMode,
		})
	}
	return out
}

// TestAuditLogRecordAndRead: entries are appended as one JSON line each, and
// RecentAudit reads them back in file order.
func TestAuditLogRecordAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nobrakes", "audit.jsonl")
	l, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range testEntries(2) {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(raw, []byte{'\n'}); got != 2 {
		t.Fatalf("file has %d lines, want 2 (one JSON line per entry)\n%s", got, raw)
	}

	all, err := RecentAudit(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].JobID != "joba" || all[1].JobID != "jobb" {
		t.Fatalf("RecentAudit = %+v, want joba then jobb in file order", all)
	}
	if all[0].Trigger != TriggerAutoMode || all[0].Window != "2026-11-03" {
		t.Fatalf("entry fields wrong: %+v", all[0])
	}

	last, err := RecentAudit(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].JobID != "jobb" {
		t.Fatalf("RecentAudit(path, 1) = %+v, want just jobb", last)
	}
}

// TestAuditLogMissingFileIsEmpty: no log yet is not an error — NoBrakes has
// simply not auto-run anything.
func TestAuditLogMissingFileIsEmpty(t *testing.T) {
	entries, err := RecentAudit(filepath.Join(t.TempDir(), "nobrakes", "audit.jsonl"), 0)
	if err != nil {
		t.Fatalf("missing file: %v, want nil", err)
	}
	if entries == nil || len(entries) != 0 {
		t.Fatalf("missing file: %v, want empty non-nil", entries)
	}
}

// TestAuditLogSkipsCorruptLine: a torn final line (a crash mid-append) must
// not hide the entries before it.
func TestAuditLogSkipsCorruptLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nobrakes", "audit.jsonl")
	l, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range testEntries(2) {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	torn, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := torn.Write(append([]byte(`{"timestamp":"2026-11-03T03:00:00Z","job_id":"tor`), '\n')); err != nil {
		t.Fatal(err)
	}
	torn.Close()
	entries, err := RecentAudit(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].JobID != "jobb" {
		t.Fatalf("corrupt line: %+v, want the 2 good entries", entries)
	}
}

// TestAuditLogNilRecordIsNoOp: a nil *AuditLog records nothing without
// panicking (the gate's "unaudited pass" path).
func TestAuditLogNilRecordIsNoOp(t *testing.T) {
	var l *AuditLog
	if err := l.Record(AuditEntry{JobID: "x"}); err != nil {
		t.Fatalf("nil Record: %v, want nil", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("nil Close: %v, want nil", err)
	}
}
