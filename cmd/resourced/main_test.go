package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// fakePub records the events the service publishes (no broker in tests).
type fakePub struct {
	mu     sync.Mutex
	events []queue.Event
}

func (f *fakePub) Publish(ctx context.Context, e queue.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakePub) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ks := make([]string, len(f.events))
	for i, e := range f.events {
		ks[i] = e.RoutingKey()
	}
	return ks
}

// fakeReadings is a fixed fit.Source.
type fakeReadings struct{ r fit.Readings }

func (f fakeReadings) Readings(ctx context.Context) (fit.Readings, error) { return f.r, nil }

func testLedger() *resources.Ledger {
	// The box of the plan: 4090 with 24 GB VRAM, 128 GB RAM; gpu and cpu
	// present (igpu off, so the RAM pool has a single source).
	return resources.New(
		resources.Device{Name: resources.DeviceGPU, Present: true, VRAMTotalMB: 24576},
		resources.Device{Name: resources.DeviceCPU, Present: true, RAMTotalMB: 131072, Cores: 16},
		resources.Device{Name: resources.DeviceIGPU, Present: false},
	)
}

// newTestService wires a service over a fresh ledger with a recording
// publisher and a fixed presence probe.
func newTestService() (*service, *fakePub) {
	pub := &fakePub{}
	svc := newService(testLedger(), pub, fakeReadings{}, prometheus.NewRegistry(),
		slog.New(slog.DiscardHandler))
	svc.detect = func(ctx context.Context) map[placement.Location]bool {
		return map[placement.Location]bool{placement.CPU: true, placement.GPU: true}
	}
	return svc, pub
}

// doWith runs one request against the given handler.
func doWith(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("%s %s: non-JSON body %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func lastIs(keys []string, want string) bool {
	return len(keys) > 0 && keys[len(keys)-1] == want
}

// TestLeaseAPI is the acceptance path: POST /lease grants when it fits,
// queues with a position when it does not, denies (409 + reason) when it
// never can; DELETE /lease/<id> releases and re-evaluates the queue.
func TestLeaseAPI(t *testing.T) {
	svc, pub := newTestService()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", svc.handleLeasePost)
	mux.HandleFunc("DELETE /lease/", svc.handleLeaseDelete)
	mux.HandleFunc("GET /state", svc.handleState)

	// It fits: 201 granted, lease.granted event, visible in /state.
	code, out := doWith(t, mux, "POST", "/lease", `{"holder":"w1","capability":"photo","device":"gpu","vram_mb":4096}`)
	if code != http.StatusCreated || out["status"] != "granted" {
		t.Fatalf("first lease: %d %v", code, out)
	}
	lease := out["lease"].(map[string]any)
	id := lease["id"].(string)
	if got := pub.keys(); !lastIs(got, "lease.granted") {
		t.Fatalf("events after grant = %v, want last lease.granted", got)
	}

	code, st := doWith(t, mux, "GET", "/state", "")
	if code != http.StatusOK {
		t.Fatalf("state: %d", code)
	}
	if leases := st["leases"].([]any); len(leases) != 1 {
		t.Fatalf("state leases = %v, want 1", leases)
	}

	// It never fits: 409 denied with a reason, lease.denied event.
	code, out = doWith(t, mux, "POST", "/lease", `{"holder":"w2","device":"tpu","vram_mb":1}`)
	if code != http.StatusConflict || out["status"] != "denied" || out["reason"] == "" {
		t.Fatalf("unknown device: %d %v", code, out)
	}
	if got := pub.keys(); !lastIs(got, "lease.denied") {
		t.Fatalf("events after deny = %v, want last lease.denied", got)
	}

	// Bad requests: 400, no ledger change.
	if code, _ = doWith(t, mux, "POST", "/lease", `{"device":"gpu"}`); code != http.StatusBadRequest {
		t.Fatalf("missing holder: %d, want 400", code)
	}
	if code, _ = doWith(t, mux, "POST", "/lease", `{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d, want 400", code)
	}

	// Fill the GPU: the next request queues (202, position), no event yet.
	if code, _ = doWith(t, mux, "POST", "/lease", `{"holder":"w3","device":"gpu","vram_mb":20480}`); code != http.StatusCreated {
		t.Fatalf("filler lease: %d", code)
	}
	before := len(pub.keys())
	code, out = doWith(t, mux, "POST", "/lease", `{"holder":"w4","device":"gpu","vram_mb":4096}`)
	if code != http.StatusAccepted || out["status"] != "queued" || out["position"] != float64(1) {
		t.Fatalf("queue: %d %v", code, out)
	}
	if got := len(pub.keys()); got != before {
		t.Fatalf("queuing must not publish an event: %v", pub.keys())
	}

	// Releasing the filler frees the GPU: 200 released, lease.released,
	// and the queued w4 is granted (lease.granted) by the re-evaluation.
	code, out = doWith(t, mux, "DELETE", "/lease/"+id, "")
	if code != http.StatusOK || out["status"] != "released" {
		t.Fatalf("release: %d %v", code, out)
	}
	if got := pub.keys(); got[len(got)-1] != "lease.granted" || got[len(got)-2] != "lease.released" {
		t.Fatalf("events after release+reeval = %v, want ...released, granted", got)
	}

	// Unknown lease: 404.
	if code, _ = doWith(t, mux, "DELETE", "/lease/nope", ""); code != http.StatusNotFound {
		t.Fatalf("release unknown: %d, want 404", code)
	}
}

// TestRefreshBrainLease: the refresh pass seeds the devices from the
// live reading, creates the brain's standing lease on the first pass
// (event), resizes it in place afterwards (no event), and a swap-window
// reading (GPU free) grants a queued lease.
func TestRefreshBrainLease(t *testing.T) {
	svc, pub := newTestService()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", svc.handleLeasePost)
	mux.HandleFunc("GET /state", svc.handleState)

	// First pass: the brain holds 18 of the 24 GB (the real box's idle
	// shape).
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 6, TotalVRAMGB: 24, FreeRAMGB: 90, TotalRAMGB: 128}}
	svc.refresh(context.Background())

	_, st := doWith(t, mux, "GET", "/state", "")
	if devs := st["devices"].([]any); len(devs) != 3 {
		t.Fatalf("devices = %v, want 3", devs)
	}
	leases := st["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("leases after first refresh = %v, want the brain standing lease", leases)
	}
	brain := leases[0].(map[string]any)
	if brain["holder"] != "brain" || brain["vram_mb"] != float64(18*1024) {
		t.Fatalf("brain lease = %v", brain)
	}
	if got := pub.keys(); !lastIs(got, "lease.granted") {
		t.Fatalf("events after first refresh = %v, want lease.granted", got)
	}

	// The conversation deepens: 22 GB used. Resized in place, no event.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 2, TotalVRAMGB: 24, FreeRAMGB: 80, TotalRAMGB: 128}}
	svc.refresh(context.Background())
	_, st = doWith(t, mux, "GET", "/state", "")
	leases = st["leases"].([]any)
	if len(leases) != 1 || leases[0].(map[string]any)["vram_mb"] != float64(22*1024) {
		t.Fatalf("after resize = %v, want one 22 GiB brain lease", leases)
	}
	if got := len(pub.keys()); got != 1 {
		t.Fatalf("resize must not publish: %v", pub.keys())
	}

	// A photo job asks for the whole GPU: it queues behind the brain.
	if code, _ := doWith(t, mux, "POST", "/lease", `{"holder":"photo","device":"gpu","vram_mb":12288}`); code != http.StatusAccepted {
		t.Fatalf("photo request: %d, want queued (202)", code)
	}
	// The swap window opens: the brain is out, the GPU measures to its
	// driver overhead, and the queued job fits.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 24, TotalVRAMGB: 24, FreeRAMGB: 100, TotalRAMGB: 128}}
	svc.refresh(context.Background())
	if got := pub.keys(); !lastIs(got, "lease.granted") {
		t.Fatalf("events in the window = %v, want the queued grant", got)
	}
}

// TestMetricsScrape: the /metrics handler exposes the ledger gauges and
// the push-style decision counters.
func TestMetricsScrape(t *testing.T) {
	ledger := testLedger()
	pub := &fakePub{}
	reg := prometheus.NewRegistry()
	svc := newService(ledger, pub, fakeReadings{}, reg, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("POST /lease", svc.handleLeasePost)

	if code, _ := doWith(t, mux, "POST", "/lease", `{"holder":"w","device":"gpu","vram_mb":4096}`); code != http.StatusCreated {
		t.Fatalf("setup lease: %d", code)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	text := string(body)
	for _, want := range []string{
		`resourced_leases_active{device="gpu"} 1`,
		`resourced_leases_vram_mb{device="gpu"} 4096`,
		`resourced_device_present{device="gpu"} 1`,
		`resourced_device_vram_total_mb{device="gpu"} 24576`,
		`resourced_queue_depth 0`,
		`resourced_lease_decisions_total{decision="granted"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
