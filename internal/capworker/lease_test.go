// Unit tests for the plan §3.6 lease integration (lease.go): the
// leaseManager state machine against a fake broker, the resourced HTTP
// broker against a fake resourced (the real HTTP API over a real
// resources.Ledger), and the catalog wiring in LeaseFor. No RabbitMQ and
// no real resourced — the live tests (capworker_live_test.go) cover the
// same state machine end-to-end against a broker.
package capworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// ---------------- the fake broker ----------------

// fakeBroker is a scriptable LeaseBroker for the state-machine tests:
// each test sets the answers, and every call is recorded.
type fakeBroker struct {
	mu       sync.Mutex
	decision LeaseDecision // Request's answer
	reqErr   error         // when set, Request fails (resourced down)
	presence LeasePresence // State's answer
	stErr    error
	released []string
	reqs     int
}

func (f *fakeBroker) Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs++
	return f.decision, f.reqErr
}

func (f *fakeBroker) Release(ctx context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, leaseID)
	return nil
}

func (f *fakeBroker) State(ctx context.Context, leaseID string) (LeasePresence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence, f.stErr
}

func (f *fakeBroker) grant(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision, f.reqErr = LeaseDecision{Status: LeaseGranted, LeaseID: id}, nil
}

func (f *fakeBroker) deny(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision, f.reqErr = LeaseDecision{Status: LeaseDenied, Reason: reason}, nil
}

func (f *fakeBroker) queue(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision, f.reqErr = LeaseDecision{Status: LeaseQueued, LeaseID: id, Position: 1}, nil
}

func (f *fakeBroker) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqErr = err
}

func (f *fakeBroker) setPresence(p LeasePresence, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence, f.stErr = p, err
}

func (f *fakeBroker) count() (reqs int, released []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs, append([]string(nil), f.released...)
}

// newTestManager builds a manager at the minimum poll (1 s). The tests
// reset lm.askedAt to the zero time between steps to bypass the
// one-request-per-poll limit.
func newTestManager(b *fakeBroker) *leaseManager {
	return newLeaseManager(b, LeaseSpec{Holder: "t", Capability: "cap.test", Device: "cpu"}, time.Second, log.New(io.Discard, "", 0))
}

// ---------------- the state machine ----------------

func TestLeaseDeniedThenGranted(t *testing.T) {
	b := &fakeBroker{}
	b.deny("no fit")
	lm := newTestManager(b)

	if lm.ensureLease(context.Background()) {
		t.Fatal("a denied lease must not hold")
	}
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none", lm.state)
	}

	b.grant("l1")
	lm.askedAt = time.Time{}
	if !lm.ensureLease(context.Background()) {
		t.Fatal("a granted lease must hold")
	}
	if lm.state != leaseHeld || lm.leaseID != "l1" {
		t.Fatalf("state = %d, lease = %q, want held l1", lm.state, lm.leaseID)
	}
}

func TestLeaseRequestErrorFailsOpen(t *testing.T) {
	b := &fakeBroker{}
	b.fail(errors.New("resourced down"))
	lm := newTestManager(b)
	if !lm.ensureLease(context.Background()) {
		t.Fatal("an unreachable resourced must fail open (the pre-26 behavior)")
	}
	if lm.state != leaseFailOpen {
		t.Fatalf("state = %d, want fail-open", lm.state)
	}
}

func TestLeaseHeldLost(t *testing.T) {
	b := &fakeBroker{}
	b.grant("l1")
	lm := newTestManager(b)
	if !lm.ensureLease(context.Background()) {
		t.Fatal("setup: the lease must hold")
	}
	lost := lm.lost()

	b.setPresence(LeaseGone, nil) // the window controller revoked it
	lm.askedAt = time.Time{}
	lm.tick(context.Background())

	if lm.holding() {
		t.Fatal("a lost lease must not hold")
	}
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none", lm.state)
	}
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("the loss channel was not closed (the consume loop would never cancel)")
	}
}

func TestLeaseHeldStateErrorKeepsLease(t *testing.T) {
	b := &fakeBroker{}
	b.grant("l1")
	lm := newTestManager(b)
	if !lm.ensureLease(context.Background()) {
		t.Fatal("setup: the lease must hold")
	}

	b.setPresence(LeaseGone, errors.New("network blip"))
	lm.askedAt = time.Time{}
	lm.tick(context.Background())

	if !lm.holding() || lm.state != leaseHeld {
		t.Fatalf("a blip mid-hold must keep the lease (state %d) — dropping it would thrash the consumer", lm.state)
	}
}

func TestLeaseQueuedThenGranted(t *testing.T) {
	b := &fakeBroker{}
	b.queue("l2")
	lm := newTestManager(b)
	if lm.ensureLease(context.Background()) {
		t.Fatal("a queued lease must not hold yet")
	}
	if lm.state != leaseQueued || lm.leaseID != "l2" {
		t.Fatalf("state = %d, lease = %q, want queued l2", lm.state, lm.leaseID)
	}

	b.setPresence(LeaseActive, nil) // a release re-evaluated the queue
	lm.askedAt = time.Time{}
	lm.tick(context.Background())

	if !lm.holding() || lm.state != leaseHeld || lm.leaseID != "l2" {
		t.Fatalf("the queued lease must become held once active (state %d, lease %q)", lm.state, lm.leaseID)
	}
}

func TestLeaseQueuedGoneReasks(t *testing.T) {
	b := &fakeBroker{}
	b.queue("l3")
	lm := newTestManager(b)
	lm.ensureLease(context.Background())

	b.setPresence(LeaseGone, nil) // withdrawn, or resourced restarted
	lm.askedAt = time.Time{}
	lm.tick(context.Background())
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none (a gone queued lease is re-asked)", lm.state)
	}

	// The re-ask is immediate — the gone path resets the rate limit.
	b.grant("l4")
	if !lm.ensureLease(context.Background()) {
		t.Fatal("the re-ask must not wait a full poll interval")
	}
	if lm.leaseID != "l4" {
		t.Fatalf("lease = %q, want l4", lm.leaseID)
	}
}

func TestLeaseQueuedStateErrorFailsOpen(t *testing.T) {
	b := &fakeBroker{}
	b.queue("l5")
	lm := newTestManager(b)
	lm.ensureLease(context.Background())

	b.setPresence(LeaseGone, errors.New("resourced down"))
	lm.askedAt = time.Time{}
	lm.tick(context.Background())

	if !lm.holding() || lm.state != leaseFailOpen {
		t.Fatalf("an unreachable resourced mid-queue must fail open (state %d)", lm.state)
	}
}

func TestLeaseFailOpenResync(t *testing.T) {
	b := &fakeBroker{}
	b.fail(errors.New("resourced down"))
	lm := newTestManager(b)
	if !lm.ensureLease(context.Background()) {
		t.Fatal("setup: the worker must be fail-open")
	}
	lost := lm.lost()

	// resourced is back and the real decision is deny: the worker must
	// stop assuming granted and tell the consume loop to cancel.
	b.deny("no fit")
	lm.askedAt = time.Time{}
	lm.tick(context.Background())

	if lm.holding() {
		t.Fatal("a fail-open worker must not hold once the real decision is deny")
	}
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none", lm.state)
	}
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("the loss channel was not closed (the consume loop would keep consuming)")
	}
}

func TestLeaseReleaseHeld(t *testing.T) {
	b := &fakeBroker{}
	b.grant("l1")
	lm := newTestManager(b)
	lm.ensureLease(context.Background())

	lm.releaseHeld(context.Background())

	if _, released := b.count(); len(released) != 1 || released[0] != "l1" {
		t.Fatalf("released = %v, want [l1]", released)
	}
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none", lm.state)
	}
	// The re-ask is deferred by a poll interval: an immediate
	// ensureLease must not fire another request.
	if lm.ensureLease(context.Background()) {
		t.Fatal("a just-released lease must not hold immediately")
	}
	if reqs, _ := b.count(); reqs != 1 {
		t.Fatalf("requests = %d, want 1 (releaseHeld defers the re-ask)", reqs)
	}
}

func TestLeaseReleaseHeldKeepsQueued(t *testing.T) {
	b := &fakeBroker{}
	b.queue("l2")
	lm := newTestManager(b)
	lm.ensureLease(context.Background())

	lm.releaseHeld(context.Background())

	if reqs, released := b.count(); len(released) != 0 || reqs != 1 {
		t.Fatalf("releaseHeld must keep a queued request (released %v, reqs %d)", released, reqs)
	}
	if lm.state != leaseQueued {
		t.Fatalf("state = %d, want queued", lm.state)
	}
}

func TestLeaseReleaseShutdown(t *testing.T) {
	b := &fakeBroker{}
	b.grant("l1")
	lm := newTestManager(b)
	lm.ensureLease(context.Background())
	lm.release(context.Background())
	if _, released := b.count(); len(released) != 1 || released[0] != "l1" {
		t.Fatalf("released = %v, want [l1]", released)
	}

	// Shutdown with a queued request withdraws it too — the ledger is
	// in-memory with no TTL, so an orphaned queue slot would sit in the
	// FIFO ahead of the restarted worker's fresh request.
	b.queue("l2")
	lm.askedAt = time.Time{}
	lm.ensureLease(context.Background())
	lm.release(context.Background())
	if _, released := b.count(); len(released) != 2 || released[1] != "l2" {
		t.Fatalf("released = %v, want [l1 l2]", released)
	}
	if lm.state != leaseNone {
		t.Fatalf("state = %d, want none", lm.state)
	}
}

func TestLeaseRateLimit(t *testing.T) {
	b := &fakeBroker{}
	b.deny("no fit")
	lm := newTestManager(b)

	lm.ensureLease(context.Background())
	lm.ensureLease(context.Background())
	if reqs, _ := b.count(); reqs != 1 {
		t.Fatalf("requests = %d, want 1 (one request per poll interval)", reqs)
	}

	lm.askedAt = time.Time{}
	lm.ensureLease(context.Background())
	if reqs, _ := b.count(); reqs != 2 {
		t.Fatalf("requests = %d, want 2 after the interval", reqs)
	}
}

// ---------------- the resourced HTTP broker ----------------

// fakeResourced is an in-process stand-in for the resourced service:
// the same HTTP API (POST /lease, DELETE /lease/<id>, GET /state) over
// a real resources.Ledger, including the re-evaluation after a release
// that the service's handler runs (a freed remainder can grant a
// waiter).
type fakeResourced struct {
	ledger *resources.Ledger
}

func newFakeResourced(ledger *resources.Ledger) http.Handler {
	return &fakeResourced{ledger: ledger}
}

func (f *fakeResourced) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/lease":
		var req resources.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad lease request: "+err.Error(), http.StatusBadRequest)
			return
		}
		out := f.ledger.Request(req)
		body := map[string]any{"status": string(out.Status)}
		if out.Lease.ID != "" {
			body["lease"] = out.Lease
		}
		switch out.Status {
		case resources.Granted:
			w.WriteHeader(http.StatusCreated)
		case resources.Queued:
			body["position"] = out.Position
			w.WriteHeader(http.StatusAccepted)
		default:
			body["reason"] = out.Reason
			w.WriteHeader(http.StatusConflict)
		}
		json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/lease/"):
		id := strings.TrimPrefix(r.URL.Path, "/lease/")
		lease, ok := f.ledger.Release(id)
		if !ok {
			http.Error(w, "no such lease", http.StatusNotFound)
			return
		}
		f.ledger.Reevaluate()
		json.NewEncoder(w).Encode(map[string]any{"status": "released", "lease": lease})
	case r.Method == http.MethodGet && r.URL.Path == "/state":
		json.NewEncoder(w).Encode(f.ledger.State())
	default:
		http.NotFound(w, r)
	}
}

// TestResourcedBroker checks the HTTP broker against the fake service:
// the three answers to a request, a State scan of the ledger, and
// releases (200 and 404 both end the call cleanly).
func TestResourcedBroker(t *testing.T) {
	ledger := resources.New(
		resources.Device{Name: resources.DeviceGPU, Present: true, VRAMTotalMB: 4096},
		resources.Device{Name: resources.DeviceCPU, Present: true, RAMTotalMB: 16 * 1024},
	)
	srv := httptest.NewServer(newFakeResourced(ledger))
	defer srv.Close()
	b := NewResourcedBroker(srv.URL)
	ctx := context.Background()

	// Fits the free VRAM → granted (201).
	d, err := b.Request(ctx, LeaseSpec{Holder: "t1", Capability: "cap.test", Device: "gpu", VRAMMB: 2048})
	if err != nil || d.Status != LeaseGranted || d.LeaseID == "" {
		t.Fatalf("first request = %+v, %v (want granted with a lease id)", d, err)
	}
	first := d.LeaseID

	// Does not fit the remainder → queued (202), visible in the state.
	d, err = b.Request(ctx, LeaseSpec{Holder: "t2", Capability: "cap.test", Device: "gpu", VRAMMB: 4096})
	if err != nil || d.Status != LeaseQueued || d.LeaseID == "" {
		t.Fatalf("second request = %+v, %v (want queued)", d, err)
	}
	if pres, err := b.State(ctx, d.LeaseID); err != nil || pres != LeaseInQueue {
		t.Fatalf("queued lease state = %d, %v (want in the queue)", pres, err)
	}

	// The granted one is active; an unknown id is gone.
	if pres, err := b.State(ctx, first); err != nil || pres != LeaseActive {
		t.Fatalf("granted lease state = %d, %v (want active)", pres, err)
	}
	if pres, err := b.State(ctx, "no-such-lease"); err != nil || pres != LeaseGone {
		t.Fatalf("unknown lease state = %d, %v (want gone)", pres, err)
	}

	// Release: 200 for ours; a 404 (already gone) is a clean no-op —
	// the broker must not report it as an error.
	if err := b.Release(ctx, first); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := b.Release(ctx, "no-such-lease"); err != nil {
		t.Fatalf("releasing an unknown lease must be nil (404 is a no-op): %v", err)
	}

	// A request that can never fit (bigger than the device) → denied
	// (409): an answer with a reason, not an error.
	if d, err := b.Request(ctx, LeaseSpec{Holder: "t3", Device: "gpu", VRAMMB: 8192}); err != nil || d.Status != LeaseDenied || d.Reason == "" {
		t.Fatalf("oversized request = %+v, %v (want denied with a reason)", d, err)
	}
}

// TestLeaseFor checks the catalog wiring: no broker URL = deliberately
// un-gated; a catalog problem is an error (the worker logs it and runs
// un-gated); a good entry yields the placement's memory spec.
func TestLeaseFor(t *testing.T) {
	// No resourced → no gating at all.
	if broker, spec, err := LeaseFor("", "", "cap.echo", "cpu", "w"); err != nil || broker != nil || spec.Capability != "" {
		t.Fatalf("unset RESOURCED_URL = un-gated: broker %v, spec %+v, err %v", broker, spec, err)
	}

	// A missing catalog is a reportable error.
	if _, _, err := LeaseFor("http://resourced:9104", filepath.Join(t.TempDir(), "nope.yaml"), "cap.echo", "cpu", "w"); err == nil {
		t.Fatal("a missing catalog must be an error (the worker logs it and runs un-gated)")
	}

	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "capabilities.yaml")
	yaml := `
capabilities:
  cap.test:
    description: "lease-for test capability"
    workers:
      - location: gpu
        model: echo
        vram_mb: 3000
        ram_mb: 256
        needs_brain_out: false
        start: ""
        stop: ""
        typical_runtime: 30s
        idle_s: 120
      - location: cpu
        model: echo
        vram_mb: 0
        ram_mb: 1024
        needs_brain_out: false
        start: ""
        stop: ""
        typical_runtime: 60s
        idle_s: 0
`
	if err := os.WriteFile(catalogPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	broker, spec, err := LeaseFor("http://resourced:9104", catalogPath, "cap.test", "gpu", "gpu-worker")
	if err != nil || broker == nil {
		t.Fatalf("good catalog: broker %v, err %v", broker, err)
	}
	if spec.Holder != "gpu-worker" || spec.Capability != "cap.test" || spec.Device != "gpu" || spec.VRAMMB != 3000 || spec.RAMMB != 256 {
		t.Fatalf("spec = %+v, want the gpu placement's footprint", spec)
	}

	if _, _, err := LeaseFor("http://resourced:9104", catalogPath, "cap.test", "igpu", "w"); err == nil {
		t.Fatal("a placement the capability lacks must be an error")
	}
	if _, _, err := LeaseFor("http://resourced:9104", catalogPath, "cap.missing", "cpu", "w"); err == nil {
		t.Fatal("an unknown capability must be an error")
	}
}
