package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
// publisher and a fixed presence probe; it returns the measurement
// overlay's file path (a temp dir, so tests never touch the real one).
func newTestService(t *testing.T) (*service, *fakePub, string) {
	t.Helper()
	pub := &fakePub{}
	log := slog.New(slog.DiscardHandler)
	measuredPath := filepath.Join(t.TempDir(), "cap-measurements.json")
	svc := newService(testLedger(), pub, fakeReadings{}, newMeasurementStore(measuredPath, log),
		prometheus.NewRegistry(), log)
	svc.detect = func(ctx context.Context) map[placement.Location]bool {
		return map[placement.Location]bool{placement.CPU: true, placement.GPU: true}
	}
	return svc, pub, measuredPath
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
	svc, pub, _ := newTestService(t)
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
	svc, pub, _ := newTestService(t)
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

// TestMeasureAPI is the measured-VRAM overlay (EVB-28): a worker reports
// its nvidia-smi delta, the store records it and emits the
// worker.<cap@device>.measured event (over=true when the catalog
// under-declared), GET /measured serves it, DELETE clears one entry, and
// the overlay persists to the file between store instances.
func TestMeasureAPI(t *testing.T) {
	svc, pub, path := newTestService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /measure", svc.handleMeasurePost)
	mux.HandleFunc("GET /measured", svc.handleMeasuredGet)
	mux.HandleFunc("DELETE /measured/", svc.handleMeasuredDelete)

	// Measures what it declared: recorded, over=false, the worker event.
	code, out := doWith(t, mux, "POST", "/measure",
		`{"capability":"image.describe","device":"gpu","declared_mb":3244,"measured_mb":3244,"holder":"image-describe-worker"}`)
	if code != http.StatusCreated || out["status"] != "recorded" {
		t.Fatalf("measure: %d %v", code, out)
	}
	if m := out["measured"].(map[string]any); m["over"] != false {
		t.Fatalf("over flag = %v, want false (measured == declared)", m)
	}
	if got := pub.keys(); !lastIs(got, "worker.image.describe@gpu.measured") {
		t.Fatalf("events after measure = %v, want the worker.measured event", got)
	}

	// The alert case: measured bigger than declared → over=true.
	code, out = doWith(t, mux, "POST", "/measure",
		`{"capability":"llm.small","device":"gpu","declared_mb":5500,"measured_mb":7168,"holder":"llm-small-worker"}`)
	if code != http.StatusCreated {
		t.Fatalf("over measure: %d", code)
	}
	if m := out["measured"].(map[string]any); m["over"] != true {
		t.Fatalf("over flag = %v, want true (7168 > 5500)", m)
	}

	// GET /measured shows both entries.
	_, st := doWith(t, mux, "GET", "/measured", "")
	meas := st["measurements"].(map[string]any)
	if len(meas) != 2 {
		t.Fatalf("overlay = %v, want 2 entries", st["measurements"])
	}
	if e := meas["llm.small@gpu"].(map[string]any); e["measured_mb"] != float64(7168) || e["over"] != true {
		t.Fatalf("llm.small entry = %v", e)
	}

	// The overlay is a file: a fresh store over the same path loads it.
	reloaded := newMeasurementStore(path, slog.New(slog.DiscardHandler))
	if got := reloaded.all(); len(got) != 2 {
		t.Fatalf("reloaded overlay = %v, want the 2 persisted entries", got)
	}

	// Bad input: 400, the store is untouched.
	if code, _ := doWith(t, mux, "POST", "/measure", `{"capability":"x","declared_mb":1,"measured_mb":1}`); code != http.StatusBadRequest {
		t.Fatalf("missing device: %d, want 400", code)
	}
	if code, _ := doWith(t, mux, "POST", "/measure", `{"capability":"x","device":"gpu","declared_mb":-1,"measured_mb":1}`); code != http.StatusBadRequest {
		t.Fatalf("negative declared: %d, want 400", code)
	}

	// DELETE clears one entry; a second DELETE of it is 404.
	if code, _ := doWith(t, mux, "DELETE", "/measured/llm.small@gpu", ""); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := doWith(t, mux, "DELETE", "/measured/llm.small@gpu", ""); code != http.StatusNotFound {
		t.Fatalf("delete again: %d, want 404", code)
	}
	_, st = doWith(t, mux, "GET", "/measured", "")
	if len(st["measurements"].(map[string]any)) != 1 {
		t.Fatalf("overlay after delete = %v, want 1 entry", st["measurements"])
	}
}

// TestMeasuredBudgetBumpsLease: a capability that measured bigger than it
// declares is charged the measured value on its next lease request (the
// catalog under-declared, the ledger uses the truth). A measured value
// smaller than the declaration never shrinks the budget.
func TestMeasuredBudgetBumpsLease(t *testing.T) {
	svc, _, _ := newTestService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /measure", svc.handleMeasurePost)
	mux.HandleFunc("POST /lease", svc.handleLeasePost)

	doWith(t, mux, "POST", "/measure",
		`{"capability":"llm.small","device":"gpu","declared_mb":5500,"measured_mb":7168}`)

	// The worker asks for its catalog value: it is charged the measured one.
	code, out := doWith(t, mux, "POST", "/lease",
		`{"holder":"llm-small","capability":"llm.small","device":"gpu","vram_mb":5500}`)
	if code != http.StatusCreated || out["status"] != "granted" {
		t.Fatalf("lease: %d %v", code, out)
	}
	if l := out["lease"].(map[string]any); l["vram_mb"] != float64(7168) {
		t.Fatalf("lease vram_mb = %v, want the measured 7168", l)
	}

	// Measured smaller than declared: the declaration stands.
	doWith(t, mux, "POST", "/measure",
		`{"capability":"image.describe","device":"gpu","declared_mb":3244,"measured_mb":2048}`)
	code, out = doWith(t, mux, "POST", "/lease",
		`{"holder":"v","capability":"image.describe","device":"gpu","vram_mb":3244}`)
	if code != http.StatusCreated {
		t.Fatalf("lease: %d %v", code, out)
	}
	if l := out["lease"].(map[string]any); l["vram_mb"] != float64(3244) {
		t.Fatalf("lease vram_mb = %v, want the declared 3244", l)
	}
}

// TestRefreshCoTenantSubtraction is the rule-5 refresh math (EVB-28): the
// measured GPU usage counts the brain AND the co-tenant workers, so the
// workers' declared leases are subtracted before sizing the brain's
// standing lease. The invariant under test: free VRAM is exactly the
// measured free (total − nvidia-smi used), whatever co-tenants run. The
// second half covers the clamp — declared budgets bigger than the
// measured usage (a window: the brain is out, the big job not loaded yet)
// floor the standing lease at 0.
func TestRefreshCoTenantSubtraction(t *testing.T) {
	svc, pub, _ := newTestService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", svc.handleLeasePost)
	mux.HandleFunc("DELETE /lease/", svc.handleLeaseDelete)
	mux.HandleFunc("GET /state", svc.handleState)

	// The small worker gets its 5500 MB lease.
	small, out := doWith(t, mux, "POST", "/lease", `{"holder":"llm-small","capability":"llm.small","device":"gpu","vram_mb":5500}`)
	if small != http.StatusCreated {
		t.Fatalf("small lease: %d %v", small, out)
	}

	// The reading: 24 GB card, 2.5 GB free — 21.5 GB of which is the
	// brain and the small worker together. Subtracting the worker's
	// declared 5500 leaves the brain's true ~16 GB, and the free
	// remainder is exactly the 2.5 GB nvidia-smi reports.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 2.5, TotalVRAMGB: 24, FreeRAMGB: 90, TotalRAMGB: 128}}
	svc.refresh(context.Background())
	_, st := doWith(t, mux, "GET", "/state", "")
	var brainMB float64
	for _, l := range st["leases"].([]any) {
		if l.(map[string]any)["holder"] == "brain" {
			brainMB = l.(map[string]any)["vram_mb"].(float64)
		}
	}
	want := float64(int64((24-2.5)*1024) - 5500) // 22016 − 5500 = 16516
	if brainMB != want {
		t.Fatalf("brain lease = %v MB, want %v (measured 21.5 GB minus the co-tenant's 5500)", brainMB, want)
	}
	// The invariant: the remainder is exactly the measured free.
	if code, _ := doWith(t, mux, "POST", "/lease", `{"holder":"fits","device":"gpu","vram_mb":2560}`); code != http.StatusCreated {
		t.Fatalf("2560 (the true free) = %d, want granted", code)
	}
	if code, out := doWith(t, mux, "POST", "/lease", `{"holder":"nope","device":"gpu","vram_mb":2561}`); code != http.StatusAccepted {
		t.Fatalf("2561 = %d %v, want queued", code, out)
	}

	// The clamp: the brain is out (a window), a 12 GB job holds a lease
	// but has not loaded yet. nvidia-smi shows only 4 GB used (driver +
	// the small worker) — smaller than the declared budgets. The
	// subtraction clamps the standing lease to 0; the job's full
	// declared budget is what the ledger reserves.
	// Nothing free (brain 16516 + small 5500 + fits 2560 = 24576 used):
	// the big job queues until the window reading lands.
	if code, out := doWith(t, mux, "POST", "/lease", `{"holder":"image-gen","capability":"image.generate","device":"gpu","vram_mb":12288}`); code != http.StatusAccepted {
		t.Fatalf("big job before the window: %d %v, want queued", code, out)
	}
	// Release the two smalls: the re-evaluation grants "nope" (2561)
	// from the freed remainder; the big job (12288) still waits.
	var ids []string
	_, st = doWith(t, mux, "GET", "/state", "")
	for _, l := range st["leases"].([]any) {
		lm := l.(map[string]any)
		if lm["holder"] == "llm-small" || lm["holder"] == "fits" {
			ids = append(ids, lm["id"].(string))
		}
	}
	for _, id := range ids {
		if code, _ := doWith(t, mux, "DELETE", "/lease/"+id, ""); code != http.StatusOK {
			t.Fatalf("release %s: %d", id, code)
		}
	}
	// The window reading: 20 GB free of 24 (4 GB used — driver overhead,
	// the big job granted but not loaded yet). The brain re-sizes down
	// and the queued big job fits the remainder: granted.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 20, TotalVRAMGB: 24, FreeRAMGB: 90, TotalRAMGB: 128}}
	svc.refresh(context.Background())
	if got := pub.keys(); !lastIs(got, "lease.granted") {
		t.Fatalf("events in the window = %v, want the queued big job granted", got)
	}
	// Now the only co-tenant is the big job (12288 declared, not loaded
	// yet): used (4096) < declared (12288), so the subtraction clamps
	// the standing lease to 0.
	var nopeID string
	_, st = doWith(t, mux, "GET", "/state", "")
	for _, l := range st["leases"].([]any) {
		if lm := l.(map[string]any); lm["holder"] == "nope" {
			nopeID = lm["id"].(string)
		}
	}
	if code, _ := doWith(t, mux, "DELETE", "/lease/"+nopeID, ""); code != http.StatusOK {
		t.Fatalf("release nope: %d", code)
	}
	svc.refresh(context.Background())
	_, st = doWith(t, mux, "GET", "/state", "")
	for _, l := range st["leases"].([]any) {
		lm := l.(map[string]any)
		switch lm["holder"] {
		case "brain":
			if lm["vram_mb"] != float64(0) {
				t.Fatalf("clamped brain lease = %v, want 0 (used 4 GB < the co-tenant's declared 12 GB)", lm["vram_mb"])
			}
		case "image-gen":
			if lm["vram_mb"] != float64(12288) {
				t.Fatalf("image-gen lease = %v, want its declared 12288", lm["vram_mb"])
			}
		}
	}
}

// TestRefreshLiveRAMGate: the refresh folds the live free RAM into the
// ledger (rule 5) — a RAM request that fits the pool but not the box
// queues, and the next refresh with more free RAM grants it.
func TestRefreshLiveRAMGate(t *testing.T) {
	svc, pub, _ := newTestService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", svc.handleLeasePost)

	// 128 GB total, 2 GB free right now.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 6, TotalVRAMGB: 24, FreeRAMGB: 2, TotalRAMGB: 128}}
	svc.refresh(context.Background())

	if code, out := doWith(t, mux, "POST", "/lease", `{"holder":"cpu-w","device":"cpu","ram_mb":4096}`); code != http.StatusAccepted {
		t.Fatalf("4 GB against 2 GB live free = %d %v, want queued", code, out)
	}
	if code, out := doWith(t, mux, "POST", "/lease", `{"holder":"cpu-s","device":"cpu","ram_mb":1024}`); code != http.StatusCreated {
		t.Fatalf("1 GB against 2 GB live free = %d %v, want granted", code, out)
	}

	// The box frees up: the refresh's re-evaluation grants the waiter.
	svc.readings = fakeReadings{fit.Readings{FreeVRAMGB: 6, TotalVRAMGB: 24, FreeRAMGB: 64, TotalRAMGB: 128}}
	svc.refresh(context.Background())
	if got := pub.keys(); !lastIs(got, "lease.granted") {
		t.Fatalf("events after the RAM freed = %v, want the queued grant", got)
	}
}

// TestMetricsScrape: the /metrics handler exposes the ledger gauges and
// the push-style decision counters.
func TestMetricsScrape(t *testing.T) {
	ledger := testLedger()
	pub := &fakePub{}
	reg := prometheus.NewRegistry()
	log := slog.New(slog.DiscardHandler)
	measured := newMeasurementStore(filepath.Join(t.TempDir(), "cap-measurements.json"), log)
	svc := newService(ledger, pub, fakeReadings{}, measured, reg, log)
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
