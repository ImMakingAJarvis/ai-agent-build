package main

import (
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

func TestRunUsage(t *testing.T) {
	if code := run([]string{"jarvis-photo"}); code != 2 {
		t.Fatalf("no-args exit = %d, want 2", code)
	}
	if code := run([]string{"jarvis-photo", "make", "--job", "bad/../id"}); code != 2 {
		t.Fatalf("bad id exit = %d, want 2", code)
	}
}

func TestRunMissingPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	if code := run([]string{"jarvis-photo", "make", "--job", "chat-1-ab"}); code != 1 {
		t.Fatalf("missing prompt exit = %d, want 1", code)
	}
}

func TestRunEmptyPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	if err := photo.WritePrompt(dir, "chat-1-ab", "   \n"); err != nil {
		t.Fatalf("WritePrompt: %v", err)
	}
	if code := run([]string{"jarvis-photo", "make", "--job", "chat-1-ab"}); code != 1 {
		t.Fatalf("empty prompt exit = %d, want 1", code)
	}
}

func TestTruncatePrompt(t *testing.T) {
	if got := truncatePrompt("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars
	if got := truncatePrompt(long, 10); got != "abcdefghij…" {
		t.Fatalf("got %q", got)
	}
}
