package wake

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNewSourceDefault(t *testing.T) {
	// Ensure WAKE_SOURCE is unset.
	os.Unsetenv("WAKE_SOURCE")

	src, err := NewSource()
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if src == nil {
		t.Fatal("NewSource() returned nil")
	}
}

func TestNewSourceHomeAssistant(t *testing.T) {
	os.Setenv("WAKE_SOURCE", "homeassistant")
	defer os.Unsetenv("WAKE_SOURCE")

	src, err := NewSource()
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if src == nil {
		t.Fatal("NewSource() returned nil")
	}
}

func TestNewSourceESP32(t *testing.T) {
	os.Setenv("WAKE_SOURCE", "esp32")
	defer os.Unsetenv("WAKE_SOURCE")

	src, err := NewSource()
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if src == nil {
		t.Fatal("NewSource() returned nil")
	}
}

func TestNewSourceUnknown(t *testing.T) {
	os.Setenv("WAKE_SOURCE", "banana")
	defer os.Unsetenv("WAKE_SOURCE")

	_, err := NewSource()
	if err == nil {
		t.Fatal("NewSource() expected error for unknown source")
	}
}

func TestEnvOr(t *testing.T) {
	os.Setenv("WAKE_TEST_KEY", "custom")
	defer os.Unsetenv("WAKE_TEST_KEY")

	if got := envOr("WAKE_TEST_KEY", "fallback"); got != "custom" {
		t.Fatalf("envOr() = %q, want %q", got, "custom")
	}

	if got := envOr("WAKE_NONEXISTENT_KEY", "fallback"); got != "fallback" {
		t.Fatalf("envOr() = %q, want %q", got, "fallback")
	}
}

func TestESP32ListenClosesChannel(t *testing.T) {
	src := NewESP32()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ch := src.Listen(ctx)

	// Wait for the channel to close.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after context cancellation")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for channel to close")
	}
}
