package agentjob

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestAppendMessagesAssignsSeq: Seq continues across calls (1, 2, then 3).
func TestAppendMessagesAssignsSeq(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AppendMessages("j1",
		LoopMessage{Role: "user", Content: json.RawMessage(`"hi"`)},
		LoopMessage{Role: "assistant", Content: json.RawMessage(`"yo"`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("first batch seq = %d,%d, want 1,2", got[0].Seq, got[1].Seq)
	}
	got2, err := s.AppendMessages("j1",
		LoopMessage{Role: "user", Content: json.RawMessage(`"again"`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got2[0].Seq != 3 {
		t.Fatalf("second batch seq = %d, want 3", got2[0].Seq)
	}
	msgs, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Errorf("msgs[%d].Seq = %d, want %d", i, m.Seq, i+1)
		}
	}
}

// TestMessagesMissingFile: no file -> nil, nil.
func TestMessagesMissingFile(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("nope")
	if err != nil {
		t.Fatalf("missing file: err = %v, want nil", err)
	}
	if msgs != nil {
		t.Fatalf("missing file: msgs = %v, want nil", msgs)
	}
}

// TestMessagesIgnoresTornLastLine: a final line cut off mid-JSON is dropped,
// the good lines before it are returned.
func TestMessagesIgnoresTornLastLine(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessages("j2",
		LoopMessage{Role: "user", Content: json.RawMessage(`"one"`)},
		LoopMessage{Role: "user", Content: json.RawMessage(`"two"`)},
	); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: append half a JSON line with no trailing newline.
	f, err := os.OpenFile(s.MessagesPath("j2"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"role":"user","content":"cut of`); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("j2")
	if err != nil {
		t.Fatalf("Messages with torn tail: err = %v, want nil", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2 (the good lines)", len(msgs))
	}
	if msgs[0].Seq != 1 || msgs[1].Seq != 2 {
		t.Errorf("seqs = %d,%d, want 1,2", msgs[0].Seq, msgs[1].Seq)
	}
}

// TestMessagesLongLine: a single 200 KB line survives (past bufio.Scanner's
// 64 KB limit).
func TestMessagesLongLine(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a", 200*1024)
	content := json.RawMessage(`"` + big + `"`)
	if _, err := s.AppendMessages("j3",
		LoopMessage{Role: "user", Content: content},
	); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("j3")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	want := `"` + big + `"`
	if string(msgs[0].Content) != want {
		t.Errorf("content did not round-trip: len got %d, want %d",
			len(msgs[0].Content), len(want))
	}
}
