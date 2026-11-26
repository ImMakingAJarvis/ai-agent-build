package fake

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// collect drains a Stream and returns every chunk it yields.
func collect(t *testing.T, s engine.Stream) []engine.Chunk {
	t.Helper()
	var out []engine.Chunk
	for {
		ch, err := s.Recv()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ch)
	}
}

func userMsg(s string) engine.ChatRequest {
	return engine.ChatRequest{Slot: 0, Messages: []engine.Message{{Role: "user", Content: s}}}
}

// TestScriptedReplies scripts two conversation keys and interleaves
// them; each key must get exactly its own scripted replies, in order.
func TestScriptedReplies(t *testing.T) {
	e := New(2)
	e.Script("a",
		Step{Reply: engine.Message{Role: "assistant", Content: "a1"}, PromptTokens: 10, OutputTokens: 2},
		Step{Reply: engine.Message{Role: "assistant", Content: "a2"}, PromptTokens: 30, OutputTokens: 4},
	)
	e.Script("b", Step{Reply: engine.Message{Role: "assistant", Content: "b1"}, PromptTokens: 20, OutputTokens: 5})

	for _, want := range []struct {
		key, text   string
		slot        int
		prompt, out int
	}{
		{"a", "a1", 0, 10, 2},
		{"b", "b1", 1, 20, 5},
		{"a", "a2", 0, 30, 4},
	} {
		req := userMsg(want.key)
		req.Slot = want.slot
		s, err := e.Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("Chat(%q): %v", want.key, err)
		}
		chunks := collect(t, s)
		if len(chunks) != 2 || !chunks[1].Done {
			t.Fatalf("key %q: got %d chunks, want a text chunk then a Done chunk", want.key, len(chunks))
		}
		if chunks[0].Text != want.text {
			t.Errorf("key %q: text chunk = %q, want %q", want.key, chunks[0].Text, want.text)
		}
		if chunks[1].Message == nil || chunks[1].Message.Content != want.text {
			t.Errorf("key %q: Done message = %+v, want content %q", want.key, chunks[1].Message, want.text)
		}
		if chunks[1].PromptTokens != want.prompt || chunks[1].OutputTokens != want.out {
			t.Errorf("key %q: tokens = %d/%d, want %d/%d",
				want.key, chunks[1].PromptTokens, chunks[1].OutputTokens, want.prompt, want.out)
		}
	}
	// The slot holds what the latest chat for it used.
	if key, _ := e.SlotKey(0); key != "a" {
		t.Errorf("slot 0 key = %q, want %q", key, "a")
	}
	if n, _ := e.SlotTokens(0); n != 34 {
		t.Errorf("slot 0 tokens = %d, want 34", n)
	}
	if key, _ := e.SlotKey(1); key != "b" {
		t.Errorf("slot 1 key = %q, want %q", key, "b")
	}
	if n, _ := e.SlotTokens(1); n != 25 {
		t.Errorf("slot 1 tokens = %d, want 25", n)
	}
	// Exhausting a key's script is an error (covered in TestNoScriptIsError).
	if _, err := e.Chat(context.Background(), userMsg("a")); err == nil ||
		!strings.Contains(err.Error(), "no scripted reply for") {
		t.Errorf("Chat after script exhausted: err = %v, want a no-scripted-reply error", err)
	}
}

// TestNoScriptIsError: a Chat for a key with no scripted replies at all,
// or one whose script is exhausted, is an error.
func TestNoScriptIsError(t *testing.T) {
	e := New(1)
	_, err := e.Chat(context.Background(), userMsg("ghost"))
	if err == nil {
		t.Fatal("Chat for an unscripted key: want an error, got nil")
	}
	if want := `fake: no scripted reply for "ghost"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}

	e.Script("a", Step{Reply: engine.Message{Role: "assistant", Content: "only"}})
	if _, err := e.Chat(context.Background(), userMsg("a")); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := e.Chat(context.Background(), userMsg("a")); err == nil ||
		!strings.Contains(err.Error(), `no scripted reply for "a"`) {
		t.Errorf("second Chat after exhaustion: err = %v, want no-scripted-reply", err)
	}
}

// TestSaveRestoreErase: SaveSlot captures the slot's key and tokens,
// RestoreSlot puts them back (even into a different slot), EraseSlot
// clears the slot, and restoring an unknown name is an error.
func TestSaveRestoreErase(t *testing.T) {
	e := New(2)
	ctx := context.Background()
	e.Script("a", Step{Reply: engine.Message{Role: "assistant", Content: "hi"}, PromptTokens: 100, OutputTokens: 50})
	if _, err := e.Chat(ctx, userMsg("a")); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	meta, err := e.SaveSlot(ctx, 0, "snap1")
	if err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	if meta.File != "snap1" || meta.Tokens != 150 {
		t.Errorf("SnapshotMeta = %+v, want File snap1 Tokens 150", meta)
	}
	if got := e.Saved(); len(got) != 1 || got["snap1"] != 150 {
		t.Errorf("Saved() = %v, want map[snap1:150]", got)
	}

	if err := e.EraseSlot(ctx, 0); err != nil {
		t.Fatalf("EraseSlot: %v", err)
	}
	if key, _ := e.SlotKey(0); key != "" {
		t.Errorf("after EraseSlot: key = %q, want empty", key)
	}
	if n, _ := e.SlotTokens(0); n != 0 {
		t.Errorf("after EraseSlot: tokens = %d, want 0", n)
	}

	if err := e.RestoreSlot(ctx, 1, "snap1"); err != nil {
		t.Fatalf("RestoreSlot: %v", err)
	}
	if key, _ := e.SlotKey(1); key != "a" {
		t.Errorf("restored slot 1 key = %q, want %q", key, "a")
	}
	if n, _ := e.SlotTokens(1); n != 150 {
		t.Errorf("restored slot 1 tokens = %d, want 150", n)
	}

	if err := e.RestoreSlot(ctx, 0, "nope"); err == nil || !strings.Contains(err.Error(), "no saved snapshot") {
		t.Errorf("RestoreSlot of an unknown name: err = %v, want an error", err)
	}
}

// TestTimelineAndClock: the clock starts at 2026-01-01 00:00 UTC, a
// Step's Delay moves it (Chat does not sleep), Advance moves it too,
// and the timeline records every Chat in order.
func TestTimelineAndClock(t *testing.T) {
	e := New(1)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !e.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", e.Now(), start)
	}
	e.Script("a",
		Step{Reply: engine.Message{Role: "assistant", Content: "one"}, Delay: 2 * time.Second},
		Step{Reply: engine.Message{Role: "assistant", Content: "two"}, Delay: time.Millisecond * 500},
	)
	if _, err := e.Chat(ctx, userMsg("a")); err != nil {
		t.Fatalf("Chat 1: %v", err)
	}
	e.Advance(time.Minute)
	if _, err := e.Chat(ctx, userMsg("a")); err != nil {
		t.Fatalf("Chat 2: %v", err)
	}

	if want := start.Add(2*time.Second + time.Minute + 500*time.Millisecond); !e.Now().Equal(want) {
		t.Errorf("Now() = %v, want %v", e.Now(), want)
	}

	tl := e.Timeline()
	if len(tl) != 2 {
		t.Fatalf("Timeline() has %d entries, want 2", len(tl))
	}
	want0 := Use{Slot: 0, Key: "a", Start: start, End: start.Add(2 * time.Second)}
	if !tl[0].Start.Equal(want0.Start) || !tl[0].End.Equal(want0.End) || tl[0].Key != want0.Key || tl[0].Slot != want0.Slot {
		t.Errorf("timeline[0] = %+v, want %+v", tl[0], want0)
	}
	want1 := Use{Slot: 0, Key: "a", Start: start.Add(2*time.Second + time.Minute), End: start.Add(2*time.Second + time.Minute + 500*time.Millisecond)}
	if !tl[1].Start.Equal(want1.Start) || !tl[1].End.Equal(want1.End) || tl[1].Key != want1.Key || tl[1].Slot != want1.Slot {
		t.Errorf("timeline[1] = %+v, want %+v", tl[1], want1)
	}
}

// TestBadSlot: a slot number outside 0..slots-1 is an error in every
// place it can appear.
func TestBadSlot(t *testing.T) {
	e := New(2)
	ctx := context.Background()
	e.Script("a", Step{Reply: engine.Message{Role: "assistant", Content: "x"}})

	req := userMsg("a")
	req.Slot = 2
	if _, err := e.Chat(ctx, req); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("Chat slot 2: err = %v, want out-of-range", err)
	}
	if _, err := e.SaveSlot(ctx, 2, "s"); err == nil {
		t.Error("SaveSlot slot 2: want an error")
	}
	if err := e.RestoreSlot(ctx, 2, "s"); err == nil {
		t.Error("RestoreSlot slot 2: want an error")
	}
	if err := e.EraseSlot(ctx, 2); err == nil {
		t.Error("EraseSlot slot 2: want an error")
	}
	if _, err := e.SlotTokens(2); err == nil {
		t.Error("SlotTokens slot 2: want an error")
	}
	if _, err := e.SlotKey(2); err == nil {
		t.Error("SlotKey slot 2: want an error")
	}
	if _, err := e.SlotTokens(-1); err == nil {
		t.Error("SlotTokens -1: want an error")
	}
}
