package agentjob

import (
	"os"
	"strings"
	"testing"
)

// TestEventsApplied: a job's events.jsonl starts absent (not applied),
// MarkEventApplied records the event (applied), and a second job has its
// own independent log.
func TestEventsApplied(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Not applied: the file does not exist yet.
	applied, err := s.EventApplied("j1", "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("EventApplied before any mark = true, want false")
	}

	if err := s.MarkEventApplied("j1", "ev-1", "op.done"); err != nil {
		t.Fatal(err)
	}
	applied, err = s.EventApplied("j1", "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("EventApplied after mark = false, want true")
	}

	// A different id in the same job is not applied.
	applied, err = s.EventApplied("j1", "ev-2")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("EventApplied(j1, ev-2) = true, want false")
	}

	// A second job is independent: ev-1 recorded for j1 says nothing
	// about j2.
	applied, err = s.EventApplied("j2", "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("EventApplied(j2, ev-1) = true, want false (separate job)")
	}
	if err := s.MarkEventApplied("j2", "ev-2", "user.message"); err != nil {
		t.Fatal(err)
	}
	applied, err = s.EventApplied("j2", "ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("EventApplied(j2, ev-1) = true after marking ev-2, want false")
	}
	applied, err = s.EventApplied("j2", "ev-2")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("EventApplied(j2, ev-2) = false after mark, want true")
	}
}

// TestEventsAppliedTornTail: a final line cut off mid-JSON (a crash during
// the mark) is treated as not applied, and the good lines before it still
// count.
func TestEventsAppliedTornTail(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEventApplied("j3", "ev-a", "op.done"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: append half a JSON line, no newline.
	f, err := os.OpenFile(s.EventsPath("j3"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"ev-b","type":"user.mess`); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := s.EventApplied("j3", "ev-b")
	if err != nil {
		t.Fatalf("EventApplied with torn tail: err = %v, want nil", err)
	}
	if applied {
		t.Fatal("EventApplied(j3, ev-b) = true on a torn line, want false")
	}
	applied, err = s.EventApplied("j3", "ev-a")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("EventApplied(j3, ev-a) = false with torn tail, want true")
	}
}

// TestMarkEventAppliedLine: the mark writes exactly one
// {"id":...,"type":...,"at":...} line.
func TestMarkEventAppliedLine(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEventApplied("j4", "ev-x", "timer.fired"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.EventsPath("j4"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("events.jsonl has %d lines, want 1", len(lines))
	}
	for _, want := range []string{`"id":"ev-x"`, `"type":"timer.fired"`, `"at":`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q missing %s", lines[0], want)
		}
	}
}
