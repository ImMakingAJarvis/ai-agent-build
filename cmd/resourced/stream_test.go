package main

// The game/stream watchdog against fakes (task EVB-29, acceptance 1): a
// stream start revokes the capability workers' GPU leases (granted AND
// queued, the brain's standing lease and every RAM lease untouched),
// turns on the GPU hold, closes an open window for the game, and emits
// the lease.released / window.closed / stream.started events; a stream
// end clears the hold and re-grants the queued GPU leases. No broker,
// no jarvis-swap, no 4090. The watcher itself is tested in
// internal/streamwatcher; these tests drive the controller's transitions
// directly (streamStarted/streamStopped).

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// newStreamTest wires a service + stream controller with a recording
// publisher and its own registry (the window controller, when attached,
// uses a second one, so the gauges never collide).
func newStreamTest(t *testing.T) (*service, *fakePub, *prometheus.Registry, *streamController) {
	t.Helper()
	pub := &fakePub{}
	log := slog.New(slog.DiscardHandler)
	reg := prometheus.NewRegistry()
	measured := newMeasurementStore(filepath.Join(t.TempDir(), "cap-measurements.json"), log)
	svc := newService(testLedger(), pub, fakeReadings{}, measured, reg, log)
	svc.detect = func(ctx context.Context) map[placement.Location]bool {
		return map[placement.Location]bool{placement.CPU: true, placement.GPU: true}
	}
	return svc, pub, reg, newStreamController(svc, reg, log)
}

// gaugeValue scrapes one gauge out of the registry by name.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.Metric[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// TestStreamGameStartClosesWindow is the acceptance path: the window is
// open for queued GPU work, two workers hold GPU leases and a third
// waits in the queue — the stream starts, the window closes for the
// game (brain comes back first), every GPU lease is revoked with its
// lease.released event, the hold turns on, and the brain's standing
// lease and the CPU (RAM-only) worker are untouched.
func TestStreamGameStartClosesWindow(t *testing.T) {
	svc, pub, reg, sc := newStreamTest(t)
	ctx := context.Background()

	svc.ledger.RefreshBrainLease(18432) // the idle brain: 18.5 GB of 24
	if got := svc.ledger.Request(resources.Request{Holder: "w-a", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w-a: %v", got)
	}
	if got := svc.ledger.Request(resources.Request{Holder: "w-b", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w-b: %v", got)
	}
	svc.ledger.SetLiveFreeRAM(16384)
	if got := svc.ledger.Request(resources.Request{Holder: "cpu-w", Device: resources.DeviceCPU, RAMMB: 4096}); got.Status != resources.Granted {
		t.Fatalf("cpu-w: %v", got)
	}
	// 2048 MB left on the card: the big waiter queues.
	if got := svc.ledger.Request(resources.Request{Holder: "w-q", Device: resources.DeviceGPU, VRAMMB: 12288}); got.Status != resources.Queued {
		t.Fatalf("w-q: %v, want queued", got)
	}
	// The window opens for the queued GPU work.
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 2}}
	svc.window = newTestWindowController(t, swap, w, pub)
	svc.window.tick(ctx)
	if !swap.open {
		t.Fatalf("setup: the window should be open")
	}

	sc.streamStarted(ctx, "sunshine")

	// The window closed for the game — the brain comes back before the
	// game gets its VRAM — with the window.closed event.
	if swap.open || swap.closeCalls != 1 {
		t.Fatalf("window: open=%v closeCalls=%d, want closed once", swap.open, swap.closeCalls)
	}
	if swap.lastClose != "game stream sunshine active" {
		t.Fatalf("close reason = %q", swap.lastClose)
	}
	ks := pub.keys()
	if countKeys(ks, "lease.released") != 3 {
		t.Errorf("lease.released events = %d, want 3 (the two granted + the queued GPU lease): %v", countKeys(ks, "lease.released"), ks)
	}
	if !lastIs(ks, "stream.started") {
		t.Fatalf("last event = %v, want stream.started", ks)
	}
	last := pub.events[len(pub.events)-1]
	if last.Kind != queue.StreamKind || last.State != "started" {
		t.Errorf("last event = %+v, want a stream.started event", last)
	}
	if last.Data["stream"] != "sunshine" || last.Data["revoked_leases"] != 3 {
		t.Errorf("stream.started data = %v, want stream=sunshine revoked_leases=3", last.Data)
	}
	// The hold is on: even a request that fits the declared remainder
	// (2048 MB) queues while the stream's VRAM is not in the math.
	if got := svc.ledger.Request(resources.Request{Holder: "late", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Queued {
		t.Fatalf("GPU request under the hold = %+v, want queued", got)
	}
	if !svc.ledger.StreamActive() {
		t.Fatal("StreamActive = false after the stream started")
	}
	// The brain's standing lease and the CPU worker survived.
	have := map[string]resources.Lease{}
	for _, l := range svc.ledger.ActiveLeases() {
		have[l.Holder] = l
	}
	if have[resources.BrainHolder].VRAMMB != 18432 {
		t.Errorf("brain lease = %d MB, want the standing 18432 untouched", have[resources.BrainHolder].VRAMMB)
	}
	if l := have["cpu-w"]; l.RAMMB != 4096 {
		t.Errorf("cpu-w lease = %+v, want its 4096 MB RAM untouched (the rule is GPU-only)", l)
	}
	for _, holder := range []string{"w-a", "w-b"} {
		if _, ok := have[holder]; ok {
			t.Errorf("%s survived the revoke", holder)
		}
	}
	if got := svc.ledger.Queued(); len(got) != 1 || got[0].Holder != "late" {
		t.Errorf("queue = %+v, want only the late request (w-q was revoked)", got)
	}
	if v := gaugeValue(t, reg, "resourced_stream_active"); v != 1 {
		t.Errorf("resourced_stream_active = %v, want 1", v)
	}
}

// TestStreamEndReleasesHold: the stream ends, the hold clears, and the
// queued GPU lease that now fits is granted by the re-evaluation
// (lease.granted before stream.stopped). The window is nil here — the
// controller must be nil-safe (no capability catalog).
func TestStreamEndReleasesHold(t *testing.T) {
	svc, pub, reg, sc := newStreamTest(t)
	ctx := context.Background()

	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 4096}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}
	sc.streamStarted(ctx, "steam-remote-play")
	if got := svc.ledger.Request(resources.Request{Holder: "waiter", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Queued {
		t.Fatalf("waiter under the hold = %v, want queued", got)
	}

	sc.streamStopped(ctx)

	if svc.ledger.StreamActive() {
		t.Fatal("StreamActive = true after the stream ended")
	}
	ks := pub.keys()
	if !lastIs(ks, "stream.stopped") {
		t.Fatalf("last event = %v, want stream.stopped", ks)
	}
	if len(ks) < 2 || ks[len(ks)-2] != "lease.granted" {
		t.Fatalf("events = %v, want the queued grant before stream.stopped", ks)
	}
	// The waiter holds its lease now.
	for _, l := range svc.ledger.ActiveLeases() {
		if l.Holder == "waiter" {
			return
		}
	}
	t.Fatalf("active leases = %+v, want the waiter granted", svc.ledger.ActiveLeases())
	if v := gaugeValue(t, reg, "resourced_stream_active"); v != 0 {
		t.Errorf("resourced_stream_active = %v, want 0", v)
	}
}

// TestStreamStartWindowAlreadyClosed: the game path and a drained tick
// race for the close — the one that loses the race is a no-op (no second
// jarvis-swap in), while the hold, the revocation and the stream event
// still happen.
func TestStreamStartWindowAlreadyClosed(t *testing.T) {
	svc, pub, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{} // already closed (the other path won)
	svc.window = newTestWindowController(t, swap, &world{}, pub)
	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}

	sc.streamStarted(ctx, "sunshine")

	if swap.closeCalls != 0 {
		t.Fatalf("closeCalls = %d, want 0 (the window was already closed)", swap.closeCalls)
	}
	for _, k := range pub.keys() {
		if k == "window.closed" {
			t.Fatal("a no-op close must not publish window.closed")
		}
	}
	if countKeys(pub.keys(), "lease.released") != 1 {
		t.Errorf("lease.released events = %v, want the one revoked GPU lease", pub.keys())
	}
	if !lastIs(pub.keys(), "stream.started") {
		t.Fatalf("last event = %v, want stream.started", pub.keys())
	}
	if !svc.ledger.StreamActive() {
		t.Fatal("the hold must be on even when the close was a no-op")
	}
}

// TestStreamStartCloseFails: a failed jarvis-swap in is logged and
// retried on the next tick (like every other failed swap) — the hold
// and the revocation still happen, and no window.closed event goes out
// for a window that is still open.
func TestStreamStartCloseFails(t *testing.T) {
	svc, pub, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{failClose: errors.New("brain start hung")}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	svc.window = newTestWindowController(t, swap, w, pub)
	svc.window.tick(ctx) // open
	if !swap.open {
		t.Fatalf("setup: the window should be open")
	}
	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}

	sc.streamStarted(ctx, "sunshine")

	if swap.closeCalls != 1 || !swap.open {
		t.Fatalf("failed close: closeCalls=%d open=%v, want attempted once, still open", swap.closeCalls, swap.open)
	}
	for _, k := range pub.keys() {
		if k == "window.closed" {
			t.Fatal("a failed close must not publish window.closed")
		}
	}
	if !svc.ledger.StreamActive() {
		t.Fatal("the hold must be on even when the close failed")
	}
	if !lastIs(pub.keys(), "stream.started") {
		t.Fatalf("last event = %v, want stream.started", pub.keys())
	}
}

// countKeys counts one routing key in an event list.
func countKeys(keys []string, want string) int {
	n := 0
	for _, k := range keys {
		if k == want {
			n++
		}
	}
	return n
}
