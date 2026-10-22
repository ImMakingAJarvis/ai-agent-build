package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAskOpenMarkerCreatesAndRemovesFile(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("JARVIS_CONF", dir)
	t.Cleanup(func() { os.Unsetenv("JARVIS_CONF") })

	cleanup, err := writeAskOpenMarker()
	if err != nil {
		t.Fatalf("writeAskOpenMarker: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup function when JARVIS_CONF is set")
	}

	// Marker file must exist.
	markerPath := filepath.Join(dir, "ask-open")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker file missing: %v", err)
	}
	if got := string(data); got != "checkpoint question open" {
		t.Fatalf("marker content = %q, want %q", got, "checkpoint question open")
	}

	// After cleanup, marker must be gone.
	cleanup()
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatal("marker file still exists after cleanup")
	}
}

func TestAskOpenMarkerNoConf(t *testing.T) {
	// Make sure JARVIS_CONF is unset.
	prev := os.Getenv("JARVIS_CONF")
	os.Unsetenv("JARVIS_CONF")
	t.Cleanup(func() {
		if prev != "" {
			os.Setenv("JARVIS_CONF", prev)
		}
	})

	cleanup, err := writeAskOpenMarker()
	if err != nil {
		t.Fatalf("writeAskOpenMarker: %v", err)
	}
	if cleanup != nil {
		t.Fatal("expected nil cleanup when JARVIS_CONF is unset")
	}
}
