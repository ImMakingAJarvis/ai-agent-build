package photo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptPath(t *testing.T) {
	want := filepath.Join("/data", "photos", "chat-1-ab.prompt")
	if got := PromptPath("/data", "chat-1-ab"); got != want {
		t.Fatalf("PromptPath = %q, want %q", got, want)
	}
}

func TestWriteReadPrompt(t *testing.T) {
	dir := t.TempDir()
	p := "A photo of a red panda in fresh snow, soft light."
	if err := WritePrompt(dir, "chat-1-ab", p); err != nil {
		t.Fatalf("WritePrompt: %v", err)
	}
	// The file must exist at the documented path.
	if _, err := os.Stat(PromptPath(dir, "chat-1-ab")); err != nil {
		t.Fatalf("prompt file missing: %v", err)
	}
	got, err := ReadPrompt(dir, "chat-1-ab")
	if err != nil {
		t.Fatalf("ReadPrompt: %v", err)
	}
	if got != p {
		t.Fatalf("ReadPrompt = %q, want %q", got, p)
	}
}

func TestReadPromptTrims(t *testing.T) {
	dir := t.TempDir()
	if err := WritePrompt(dir, "chat-1-ab", "  a sunset\n"); err != nil {
		t.Fatalf("WritePrompt: %v", err)
	}
	if got, _ := ReadPrompt(dir, "chat-1-ab"); got != "a sunset" {
		t.Fatalf("ReadPrompt = %q, want %q", got, "a sunset")
	}
}

func TestReadPromptMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPrompt(dir, "chat-1-xx"); err == nil {
		t.Fatalf("expected error for missing prompt file")
	}
}

func TestFollowupPath(t *testing.T) {
	want := filepath.Join("/data", "photos", "chat-1-ab.followup")
	if got := FollowupPath("/data", "chat-1-ab"); got != want {
		t.Fatalf("FollowupPath = %q, want %q", got, want)
	}
}

func TestWriteReadFollowup(t *testing.T) {
	dir := t.TempDir()
	body := "driver: local\npriority: 900\ntitle: caption it\n\nCaption {{image}}."
	if err := WriteFollowup(dir, "chat-1-ab", body); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if _, err := os.Stat(FollowupPath(dir, "chat-1-ab")); err != nil {
		t.Fatalf("followup file missing: %v", err)
	}
	got, err := ReadFollowup(dir, "chat-1-ab")
	if err != nil {
		t.Fatalf("ReadFollowup: %v", err)
	}
	if got != body {
		t.Fatalf("ReadFollowup = %q, want %q", got, body)
	}
}

func TestReadFollowupMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFollowup(dir, "chat-1-xx"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFollowup = %v, want os.ErrNotExist", err)
	}
}

func TestParseFollowupHeadersAndText(t *testing.T) {
	body := "driver: local\nclass: build\npriority: 900\ntitle: caption the new photo\n\nCaption {{image}} and post it to #gallery."
	fu, err := ParseFollowup(body)
	if err != nil {
		t.Fatalf("ParseFollowup: %v", err)
	}
	if fu.Driver != "local" || fu.Class != "build" || fu.Priority != 900 ||
		fu.Title != "caption the new photo" {
		t.Fatalf("headers wrong: %+v", fu)
	}
	if fu.Instructions != "Caption {{image}} and post it to #gallery." {
		t.Fatalf("Instructions = %q", fu.Instructions)
	}
}

func TestParseFollowupNoHeader(t *testing.T) {
	fu, err := ParseFollowup("just do the thing with {{image}}")
	if err != nil {
		t.Fatalf("ParseFollowup: %v", err)
	}
	if fu.Instructions != "just do the thing with {{image}}" {
		t.Fatalf("Instructions = %q", fu.Instructions)
	}
	if fu.Driver != "" || fu.Class != "" || fu.Priority != 0 || fu.Title != "" {
		t.Fatalf("expected empty headers: %+v", fu)
	}
}

func TestParseFollowupUnknownKeyIsText(t *testing.T) {
	// A header line that is not a known key is free text: it starts the
	// instruction and everything after it is text too.
	body := "post this: the gallery link\nand this line"
	fu, err := ParseFollowup(body)
	if err != nil {
		t.Fatalf("ParseFollowup: %v", err)
	}
	if fu.Instructions != "post this: the gallery link\nand this line" {
		t.Fatalf("Instructions = %q", fu.Instructions)
	}
}

func TestParseFollowupKnownHeaderThenColonText(t *testing.T) {
	// A known header followed by a colon-bearing text line: the text line
	// ends the header.
	body := "priority: 50\nCaption it: keep the colons"
	fu, err := ParseFollowup(body)
	if err != nil {
		t.Fatalf("ParseFollowup: %v", err)
	}
	if fu.Priority != 50 {
		t.Fatalf("Priority = %d, want 50", fu.Priority)
	}
	if fu.Instructions != "Caption it: keep the colons" {
		t.Fatalf("Instructions = %q", fu.Instructions)
	}
}

func TestParseFollowupBadPriority(t *testing.T) {
	if _, err := ParseFollowup("priority: high\ndo the thing"); err == nil {
		t.Fatalf("expected error for non-numeric priority")
	}
}

func TestParseFollowupEmptyInstructions(t *testing.T) {
	if _, err := ParseFollowup("driver: local\n"); err == nil {
		t.Fatalf("expected error for header-only body")
	}
}

func TestStandardInstructions(t *testing.T) {
	s := StandardInstructions("chat-1-ab")
	if !strings.Contains(s, "jarvis-photo make --job chat-1-ab") {
		t.Fatalf("missing executor command: %s", s)
	}
	if !strings.Contains(s, "no model round") {
		t.Fatalf("missing code-executor note: %s", s)
	}
}

func TestSizePath(t *testing.T) {
	want := filepath.Join("/data", "photos", "chat-1-ab.size")
	if got := SizePath("/data", "chat-1-ab"); got != want {
		t.Fatalf("SizePath = %q, want %q", got, want)
	}
}

func TestWriteReadSize(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSize(dir, "chat-1-ab", "16x9\n"); err != nil {
		t.Fatalf("WriteSize: %v", err)
	}
	got, err := ReadSize(dir, "chat-1-ab")
	if err != nil {
		t.Fatalf("ReadSize: %v", err)
	}
	if got != "16x9" {
		t.Fatalf("ReadSize = %q, want %q", got, "16x9")
	}
}

func TestReadSizeMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadSize(dir, "chat-1-xx"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist for missing size file, got %v", err)
	}
}

func TestRefPath(t *testing.T) {
	want := filepath.Join("/data", "photos", "chat-1-ab.png")
	if got := RefPath("/data", "chat-1-ab"); got != want {
		t.Fatalf("RefPath = %q, want %q", got, want)
	}
}

func TestWriteReadRef(t *testing.T) {
	dir := t.TempDir()
	b := []byte{0x89, 'P', 'N', 'G', 9, 13, 10, 13, 42}
	if err := WriteRefBytes(dir, "chat-1-ab", b); err != nil {
		t.Fatalf("WriteRefBytes: %v", err)
	}
	got, err := os.ReadFile(RefPath(dir, "chat-1-ab"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(b) {
		t.Errorf("ref bytes = %v, want %v", got, b)
	}
	// RefFile resolves the stored path when the sidecar exists...
	p, err := RefFile(dir, "chat-1-ab")
	if err != nil || p != RefPath(dir, "chat-1-ab") {
		t.Errorf("RefFile = %q (err %v)", p, err)
	}
	// ...and reports a missing job as not existing (the plain photo case).
	if _, err := RefFile(dir, "chat-9-zz"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("RefFile on missing job = err %v, want ErrNotExist", err)
	}
}
