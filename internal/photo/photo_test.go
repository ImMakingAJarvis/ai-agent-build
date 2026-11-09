package photo

import (
	"errors"
	"os"
	"path/filepath"
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
