package vlmserver

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeBinary writes a coprocess that ignores SIGINT and survives until
// SIGKILL — the shape that exposed the run-4 wedge: Stop's SIGINT phase
// times out, and the SIGKILL phase must still bound the wait.
func fakeBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-server.sh")
	script := "#!/bin/sh\ntrap '' INT\nexec sleep 300\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return p
}

// healthPort stands up a /health endpoint with the given status and
// returns its port; the server's Binary is the fake coprocess (it never
// serves anything itself — the health probe is what Start waits on).
func healthPort(t *testing.T, status int) int {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(hs.Close)
	return hs.Listener.Addr().(*net.TCPAddr).Port
}

// TestStopBoundedWhenProcessIgnoresSIGINT is the run-4 regression: the
// gate's idle unload called Stop on a live coprocess, and Stop hung for
// 26 minutes because the Wait goroutine's send landed on the nilled
// (claimed) channel — a send to a nil channel blocks forever. Stop must
// return within its SIGINT grace plus the SIGKILL, whatever the process
// does with the SIGINT.
func TestStopBoundedWhenProcessIgnoresSIGINT(t *testing.T) {
	s := &Server{
		Binary:        fakeBinary(t),
		Model:         "/nonexistent/model.gguf", // the fake ignores its args
		Port:          healthPort(t, http.StatusOK),
		HealthTimeout: 5 * time.Second,
		StopGrace:     300 * time.Millisecond,
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop() // idempotent: after the explicit Stop below this is a no-op

	done := make(chan error, 1)
	go func() { done <- s.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return within 30 s — the Wait goroutine is stuck (a send to the claimed channel's nil field blocks forever)")
	}
	if s.Started() {
		t.Error("server still started after Stop")
	}
}

// TestStartHealthTimeoutStopsBounded covers the other live-Stop caller:
// Start's health timeout path stops the coprocess it just launched. With
// an unhealthy /health the process stays up and the timeout Stop hits the
// same SIGINT-ignoring shape.
func TestStartHealthTimeoutStopsBounded(t *testing.T) {
	s := &Server{
		Binary:        fakeBinary(t),
		Model:         "/nonexistent/model.gguf",
		Port:          healthPort(t, http.StatusServiceUnavailable),
		HealthTimeout: 1 * time.Second,
		StopGrace:     300 * time.Millisecond,
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Start (health-timeout path) did not return — its internal Stop wedged")
	}
	if err == nil {
		t.Fatal("Start: expected the health-timeout error, got nil")
	}
	if d := time.Since(started); d > 20*time.Second {
		t.Fatalf("Start took %s — the Stop in the timeout path is not bounded", d)
	}
	if s.Started() {
		t.Error("server still started after the timed-out Start")
	}
}
