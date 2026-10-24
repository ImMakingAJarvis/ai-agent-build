package hermes

import (
	"context"
	"log"
	"testing"
)

func TestNewRunner_Defaults(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	if r.agent == nil {
		t.Fatal("expected non-nil agent")
	}
	if r.search == nil {
		t.Fatal("expected non-nil search")
	}
	if r.delegate == nil {
		t.Fatal("expected non-nil delegator")
	}
	if r.router == nil {
		t.Fatal("expected non-nil router")
	}
}

func TestNewRunner_CustomModel(t *testing.T) {
	r := NewRunner(RunnerConfig{Model: "custom-model"})
	if r.cfg.Model != "custom-model" {
		t.Errorf("expected model 'custom-model', got %q", r.cfg.Model)
	}
}

func TestStubSearch_Disabled(t *testing.T) {
	s := newStubSearch(false)
	results, err := s.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results when disabled, got %d", len(results))
	}
}

func TestStubSearch_Enabled(t *testing.T) {
	s := newStubSearch(true)
	results, err := s.Search(context.Background(), "test query", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one stub result")
	}
	if results[0].Title == "" {
		t.Error("expected non-empty title")
	}
}

func TestStubDelegator_Disabled(t *testing.T) {
	d := newStubDelegator(false, "")
	_, err := d.Delegate(context.Background(), DelegationTask{Title: "test"})
	if err == nil {
		t.Error("expected error when delegation is disabled")
	}
}

func TestStubDelegator_Delegate(t *testing.T) {
	d := newStubDelegator(true, "http://localhost:5556")
	id, err := d.Delegate(context.Background(), DelegationTask{
		Title:       "test_task",
		Description: "a test task",
		Priority:    5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty task ID")
	}

	// Verify we can look up the status.
	status, err := d.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("status lookup failed: %v", err)
	}
	if status.ID != id {
		t.Errorf("status ID mismatch: got %q, want %q", status.ID, id)
	}
	if status.State != "pending" {
		t.Errorf("expected state 'pending', got %q", status.State)
	}
}

func TestStubDelegator_StatusNotFound(t *testing.T) {
	d := newStubDelegator(true, "")
	_, err := d.Status(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for unknown task ID")
	}
}

func TestStubRouter_Route(t *testing.T) {
	r := newStubRouter(nil)
	err := r.Route(context.Background(), "discord", "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStubRouter_Destinations(t *testing.T) {
	r := newStubRouter(nil)
	dests := r.Destinations()
	if len(dests) == 0 {
		t.Error("expected at least one default destination")
	}

	// With custom rules.
	rules := map[string]string{"discord": "primary", "webhook": "https://example.com"}
	r2 := newStubRouter(rules)
	dests2 := r2.Destinations()
	if len(dests2) != 2 {
		t.Errorf("expected 2 destinations, got %d", len(dests2))
	}
}

func TestRunner_Getters(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	if r.Agent() == nil {
		t.Error("expected non-nil Agent()")
	}
	if r.SearchEngine() == nil {
		t.Error("expected non-nil SearchEngine()")
	}
	if r.Delegator() == nil {
		t.Error("expected non-nil Delegator()")
	}
	if r.Router() == nil {
		t.Error("expected non-nil Router()")
	}
}

func TestRunner_LogOutput(t *testing.T) {
	// Verify custom log output is accepted.
	var buf []byte
	custom := log.New(&nopWriter{}, "", 0)
	_ = NewRunner(RunnerConfig{LogOutput: custom})
	_ = buf // suppress unused warning
}

// nopWriter discards all writes (for testing custom log output).
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNewHTTPSearch(t *testing.T) {
	s := NewHTTPSearch("http://localhost:8080/search", 0)
	if s == nil {
		t.Fatal("expected non-nil HTTP search")
	}
}

func TestNewHTTPDelegator(t *testing.T) {
	d := NewHTTPDelegator("http://localhost:8080/delegate", 0)
	if d == nil {
		t.Fatal("expected non-nil HTTP delegator")
	}
}
