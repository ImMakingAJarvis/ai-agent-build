package capworker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func TestLocationPriority(t *testing.T) {
	cases := map[string]int{
		"gpu":         10,
		"igpu":        5,
		"cpu":         1,
		"remote:box":  1,
		"":            1, // a worker that does not say where it runs is a cheap local one
		"unknown-loc": 1,
	}
	for loc, want := range cases {
		if got := LocationPriority(loc); got != want {
			t.Errorf("LocationPriority(%q) = %d, want %d", loc, got, want)
		}
	}
}

func TestEffectivePriority(t *testing.T) {
	w := New(Config{Cap: "x", Name: "x", Model: "x", Location: "gpu"})
	if got := w.effectivePriority(); got != 10 {
		t.Errorf("gpu worker priority = %d, want 10", got)
	}
	w = New(Config{Cap: "x", Name: "x", Model: "x", Location: "cpu", Priority: 7})
	if got := w.effectivePriority(); got != 7 {
		t.Errorf("explicit Priority override = %d, want 7", got)
	}
}

func TestNewDefaults(t *testing.T) {
	w := New(Config{Cap: "x", Name: "x", Model: "x"})
	if w.cfg.OutDirBase == "" {
		t.Error("OutDirBase default not filled in")
	}
	if !w.isLoaded() {
		t.Error("a worker starts loaded")
	}
	// No lease broker = un-gated: the worker never asks for a lease.
	if w.lease != nil {
		t.Error("a worker without a lease broker must have no lease manager (un-gated)")
	}
	// Heartbeats are on by default only when an events publisher exists.
	if w.cfg.HeartbeatS != 0 {
		t.Errorf("HeartbeatS without Events = %v, want 0", w.cfg.HeartbeatS)
	}
	withEvents := New(Config{Cap: "x", Name: "x", Model: "x", Events: &queue.EventPublisher{}})
	if withEvents.cfg.HeartbeatS != 30*time.Second {
		t.Errorf("HeartbeatS with Events = %v, want the 30s default", withEvents.cfg.HeartbeatS)
	}
}

// TestMetricsHandler scrapes the worker's own registry: the outcome series
// exist before the first delivery, and an observed delivery shows up.
func TestMetricsHandler(t *testing.T) {
	m := NewMetrics()
	m.Observe("ok", 1200*time.Millisecond)
	m.SetLoaded(false)
	m.SetInflight(1)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	body, err := io.ReadAll(svrGet(t, srv.URL))
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`capworker_deliveries_total{outcome="ok"} 1`,
		`capworker_loaded 0`,
		`capworker_inflight 1`,
		`capworker_handler_duration_seconds_bucket`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
}

func svrGet(t *testing.T, url string) io.ReadCloser {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp.Body
}
