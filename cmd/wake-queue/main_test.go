package main

import (
	"context"
	"testing"
	"time"
)

// TestRunErrorsWhenWakeSourceUnknown confirms run() fails fast with
// an invalid WAKE_SOURCE value rather than hanging.
func TestRunErrorsWhenWakeSourceUnknown(t *testing.T) {
	t.Setenv("WAKE_SOURCE", "nonexistent")
	// run() tries to connect to RabbitMQ first, then creates the wake source.
	// If RabbitMQ isn't running, it errors on connect; if it is running,
	// it errors on NewSource. Either way, it should not block.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run()
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error for unknown wake source")
		}
		// Acceptable: either "rabbitmq connect" or "wake source" error.
	case <-ctx.Done():
		t.Fatal("run() did not return within timeout")
	}
}
