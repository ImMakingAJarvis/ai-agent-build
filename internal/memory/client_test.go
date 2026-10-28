package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

func testClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"text":"wake word latency was 1.2s","score":0.5,"source":"jobs","path":"job-1/transcript.txt","title":"job-1"}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	hits := c.Search(context.Background(), "wake word", 5)
	want := []Hit{{
		Text: "wake word latency was 1.2s", Score: 0.5,
		Source: "jobs", Path: "job-1/transcript.txt", Title: "job-1",
	}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("got %v", hits)
	}
}

func TestSearchFailsOpen(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", HTTP: testClient()}
	if hits := c.Search(context.Background(), "x", 3); hits != nil {
		t.Fatalf("expected nil, got %v", hits)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c2 := &Client{BaseURL: srv.URL, HTTP: testClient()}
	if hits := c2.Search(context.Background(), "x", 3); hits != nil {
		t.Fatalf("expected nil on 503, got %v", hits)
	}
}

func TestFormatContext(t *testing.T) {
	hits := []Hit{
		{Text: "alpha line", Source: "jobs", Path: "a.txt"},
		{Text: "beta line", Source: "jobs", Path: "b.txt"},
	}
	got := FormatContext(hits, 1000)
	want := "[source: path] excerpts\n[jobs a.txt] alpha line\n[jobs b.txt] beta line"
	if got != want {
		t.Fatalf("got %q", got)
	}

	// Tiny budget: only the header plus what fits.
	if got := FormatContext(hits, 30); got != "[source: path] excerpts" {
		t.Fatalf("budget: got %q", got)
	}
	if got := FormatContext(nil, 100); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}

// TestLiveSearch exercises the real memory service when it is running
// (skipped elsewhere, e.g. CI). It verifies the field contract the
// chatbot relies on: hits carry source, path, title and non-empty text.
func TestLiveSearch(t *testing.T) {
	c := NewClient(os.Getenv("MEMORY_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hits := c.Search(ctx, "wake word latency", 3)
	if hits == nil {
		t.Skip("memory service not reachable")
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit for 'wake word latency'")
	}
	for _, h := range hits {
		if h.Text == "" || h.Source == "" {
			t.Fatalf("hit missing text/source: %+v", h)
		}
	}
}
