package main

// The game/stream watchdog against fakes (task EVB-29, acceptance 1): a
// GAME start revokes the capability workers' GPU leases (granted AND
// queued, the brain's standing lease and every RAM lease untouched),
// turns on the GPU hold, and stops the brain (jarvis-swap out --game,
// window.opened for:["game"]); a game end brings the brain back
// (window.closed, only when the window is the game's) and re-grants the
// queued GPU leases. A DESKTOP stream (Sunshine) takes no GPU action at
// all — no hold, no revocation, no window. No broker, no jarvis-swap, no
// 4090. The watcher itself is tested in internal/streamwatcher; these
// tests drive the controller's transitions directly
// (streamStarted/streamStopped/gameKeepalive).

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
	"github.com/ImMakingAJarvis/ai-agent-build/internal/streamwatcher"
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

// TestStreamDesktopStartDoesNothing: a Sunshine stream is plain desktop
// streaming (NVENC only) — the brain stays up and the workers keep
// their leases. The controller only bumps the gauge and publishes
// stream.started{game:false}; the end does nothing either.
func TestStreamDesktopStartDoesNothing(t *testing.T) {
	svc, pub, reg, sc := newStreamTest(t)
	ctx := context.Background()

	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}
	swap := &fakeSwap{}
	svc.window = newTestWindowController(t, swap, &world{}, pub)

	sc.streamStarted(ctx, streamwatcher.SunshineStream)

	// No GPU action: no hold, no revocation, no window.
	if svc.ledger.StreamActive() {
		t.Fatal("StreamActive = true after a desktop stream — the hold must stay off")
	}
	if got := svc.ledger.Request(resources.Request{Holder: "late", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("GPU request after the desktop stream = %+v, want granted (no hold)", got)
	}
	for _, l := range svc.ledger.ActiveLeases() {
		if l.Holder == "w" {
			break
		}
	}
	found := false
	for _, l := range svc.ledger.ActiveLeases() {
		if l.Holder == "w" {
			found = true
		}
	}
	if !found {
		t.Fatal("the worker's GPU lease must survive a desktop stream")
	}
	if swap.openCalls+swap.openGameCalls+swap.closeCalls != 0 {
		t.Fatalf("a desktop stream must not touch the window (open=%d openGame=%d close=%d)",
			swap.openCalls, swap.openGameCalls, swap.closeCalls)
	}
	ks := pub.keys()
	if !lastIs(ks, "stream.started") {
		t.Fatalf("last event = %v, want stream.started", ks)
	}
	last := pub.events[len(pub.events)-1]
	if last.Data["stream"] != "sunshine" || last.Data["game"] != false {
		t.Errorf("stream.started data = %v, want stream=sunshine game=false", last.Data)
	}
	if v := gaugeValue(t, reg, "resourced_stream_active"); v != 1 {
		t.Errorf("resourced_stream_active = %v, want 1", v)
	}

	// ...and the end takes no action either.
	sc.streamStopped(ctx)
	ks = pub.keys()
	if !lastIs(ks, "stream.stopped") {
		t.Fatalf("last event = %v, want stream.stopped", ks)
	}
	last = pub.events[len(pub.events)-1]
	if last.Data["game"] != false {
		t.Errorf("stream.stopped data = %v, want game=false", last.Data)
	}
	if v := gaugeValue(t, reg, "resourced_stream_active"); v != 0 {
		t.Errorf("resourced_stream_active = %v, want 0", v)
	}
	if swap.openCalls+swap.openGameCalls+swap.closeCalls != 0 {
		t.Fatal("a desktop stream end must not touch the window")
	}
	if svc.ledger.StreamActive() {
		t.Fatal("StreamActive = true after the desktop stream ended")
	}
}

// TestStreamGameStartStopsBrain is the acceptance path: two workers hold
// GPU leases and a third waits in the queue, the brain sits at 18.5 GB
// — the Steam game starts, the window opens FOR the game (jarvis-swap
// out --game, for:["game"], no photo services), every GPU lease is
// revoked with its lease.released event, the hold turns on, and the
// brain's standing lease and the CPU (RAM-only) worker are untouched.
func TestStreamGameStartStopsBrain(t *testing.T) {
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
	// No window open yet (the game is the first claim on the 4090).
	swap := &fakeSwap{}
	svc.window = newTestWindowController(t, swap, &world{}, pub)

	sc.streamStarted(ctx, streamwatcher.SteamGame)

	// The window opened for the game — the brain went out, no photo
	// services — with the window.opened event.
	if swap.openGameCalls != 1 || !swap.open || len(swap.forCaps) != 1 || swap.forCaps[0] != "game" {
		t.Fatalf("window: openGame=%d open=%v for=%v, want one open --game for [game]",
			swap.openGameCalls, swap.open, swap.forCaps)
	}
	if swap.lastOpenGame != "steam game active (steam-game)" {
		t.Fatalf("open reason = %q", swap.lastOpenGame)
	}
	ks := pub.keys()
	if countKeys(ks, "window.opened") != 1 {
		t.Errorf("window.opened events = %d, want 1: %v", countKeys(ks, "window.opened"), ks)
	}
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
	if last.Data["stream"] != "steam-game" || last.Data["game"] != true || last.Data["revoked_leases"] != 3 {
		t.Errorf("stream.started data = %v, want stream=steam-game game=true revoked_leases=3", last.Data)
	}
	// The hold is on: even a request that fits the declared remainder
	// (2048 MB) queues while the game's VRAM is not in the math.
	if got := svc.ledger.Request(resources.Request{Holder: "late", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Queued {
		t.Fatalf("GPU request under the hold = %+v, want queued", got)
	}
	if !svc.ledger.StreamActive() {
		t.Fatal("StreamActive = false after the game started")
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

// TestStreamGameEndBringsBrainBack: the game ends — the game's window
// closes (brain back), the hold clears, and the queued GPU lease that
// now fits is granted by the re-evaluation (lease.granted before
// stream.stopped{game:true}).
func TestStreamGameEndBringsBrainBack(t *testing.T) {
	svc, pub, reg, sc := newStreamTest(t)
	ctx := context.Background()

	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 4096}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}
	swap := &fakeSwap{}
	svc.window = newTestWindowController(t, swap, &world{}, pub)

	sc.streamStarted(ctx, streamwatcher.SteamGame)
	if swap.openGameCalls != 1 || !swap.open {
		t.Fatalf("setup: the game window should be open (openGame=%d open=%v)", swap.openGameCalls, swap.open)
	}
	if got := svc.ledger.Request(resources.Request{Holder: "waiter", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Queued {
		t.Fatalf("waiter under the hold = %v, want queued", got)
	}

	sc.streamStopped(ctx)

	// The game's window closed — the brain is coming back.
	if swap.open || swap.closeCalls != 1 || swap.lastClose != "steam game ended" {
		t.Fatalf("window: open=%v closes=%d last=%q, want closed once for the game end",
			swap.open, swap.closeCalls, swap.lastClose)
	}
	if svc.ledger.StreamActive() {
		t.Fatal("StreamActive = true after the game ended")
	}
	ks := pub.keys()
	if !lastIs(ks, "stream.stopped") {
		t.Fatalf("last event = %v, want stream.stopped", ks)
	}
	last := pub.events[len(pub.events)-1]
	if last.Data["game"] != true {
		t.Errorf("stream.stopped data = %v, want game=true", last.Data)
	}
	if countKeys(ks, "window.closed") != 1 {
		t.Errorf("window.closed events = %d, want 1: %v", countKeys(ks, "window.closed"), ks)
	}
	if len(ks) < 2 || ks[len(ks)-2] != "lease.granted" {
		t.Fatalf("events = %v, want the queued grant before stream.stopped", ks)
	}
	if v := gaugeValue(t, reg, "resourced_stream_active"); v != 0 {
		t.Errorf("resourced_stream_active = %v, want 0", v)
	}
	found := false
	for _, l := range svc.ledger.ActiveLeases() {
		if l.Holder == "waiter" {
			found = true
		}
	}
	if !found {
		t.Fatalf("active leases = %+v, want the waiter granted", svc.ledger.ActiveLeases())
	}
}

// TestStreamGameStartBrainOutFails: a failed jarvis-swap out --game (the
// round guard — invariant 1) is logged and retried by the keepalive —
// the hold and the revocation still happen, and no window.opened event
// goes out for a brain that never went out.
func TestStreamGameStartBrainOutFails(t *testing.T) {
	svc, pub, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{failOpenGame: errors.New("round running — the guard holds")}
	svc.window = newTestWindowController(t, swap, &world{}, pub)
	svc.ledger.RefreshBrainLease(18432)
	if got := svc.ledger.Request(resources.Request{Holder: "w", Device: resources.DeviceGPU, VRAMMB: 2048}); got.Status != resources.Granted {
		t.Fatalf("w: %v", got)
	}

	sc.streamStarted(ctx, streamwatcher.SteamGame)

	if swap.openGameCalls != 1 {
		t.Fatalf("openGameCalls = %d, want one (failed) attempt", swap.openGameCalls)
	}
	for _, k := range pub.keys() {
		if k == "window.opened" {
			t.Fatal("a failed brain-out must not publish window.opened")
		}
	}
	if !svc.ledger.StreamActive() {
		t.Fatal("the hold must be on even when the brain-out failed")
	}
	if countKeys(pub.keys(), "lease.released") != 1 {
		t.Errorf("lease.released events = %v, want the one revoked GPU lease", pub.keys())
	}
	if !lastIs(pub.keys(), "stream.started") {
		t.Fatalf("last event = %v, want stream.started", pub.keys())
	}
}

// TestGameKeepaliveReacquires: the round guard aborted the brain-out at
// game start (window closed, brain still up); the 60 s keepalive
// re-acquires so the brain goes out the moment it legally may.
func TestGameKeepaliveReacquires(t *testing.T) {
	svc, _, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{failOpenGame: errors.New("round running")}
	svc.window = newTestWindowController(t, swap, &world{}, &fakePub{})
	svc.ledger.RefreshBrainLease(18432)

	sc.streamStarted(ctx, streamwatcher.SteamGame)
	if swap.openGameCalls != 1 || swap.open {
		t.Fatalf("setup: the brain-out should have failed (calls=%d open=%v)", swap.openGameCalls, swap.open)
	}

	// The round is done: the probe says the brain is up, the swap now
	// succeeds.
	swap.mu.Lock()
	swap.failOpenGame = nil
	swap.mu.Unlock()
	svc.window.brainUp = func(context.Context) bool { return true }
	sc.probeBrain = func(context.Context) bool { return true }

	sc.gameKeepalive(ctx)

	if swap.openGameCalls != 2 || !swap.open || len(swap.forCaps) != 1 || swap.forCaps[0] != "game" {
		t.Fatalf("keepalive: calls=%d open=%v for=%v, want the brain out for the game",
			swap.openGameCalls, swap.open, swap.forCaps)
	}
}

// TestGameKeepaliveRearmsMaintenance: the window is open for the game
// and the brain is out — the keepalive's only job is to keep the 4 h
// watchdog-maintenance window alive (a session longer than that would
// re-arm the brain under the game). While the maintenance window is
// still alive the keepalive is quiet.
func TestGameKeepaliveRearmsMaintenance(t *testing.T) {
	svc, _, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{}
	svc.window = newTestWindowController(t, swap, &world{}, &fakePub{})
	svc.ledger.RefreshBrainLease(18432)

	sc.streamStarted(ctx, streamwatcher.SteamGame)
	if !swap.open {
		t.Fatalf("setup: the game window should be open")
	}
	svc.window.brainUp = func(context.Context) bool { return false } // the brain is out
	sc.probeBrain = func(context.Context) bool { return false }

	rearms := 0
	sc.maintainAlive = func() bool { return false }
	sc.maintainRearm = func() error { rearms++; return nil }

	sc.gameKeepalive(ctx)
	if rearms != 1 {
		t.Fatalf("re-arms = %d, want 1 (the maintenance window lapsed)", rearms)
	}

	// While the maintenance window is still alive, the keepalive is quiet.
	sc.maintainAlive = func() bool { return true }
	sc.gameKeepalive(ctx)
	if rearms != 1 {
		t.Fatalf("re-arms = %d, want still 1 (maintenance still alive)", rearms)
	}
}

// TestGameKeepaliveIgnoresWatchdogState: a closed marker with the brain
// down is the watchdog's state (a brain restart or a manual swap in),
// not the game's — the keepalive logs and stays out of it.
func TestGameKeepaliveIgnoresWatchdogState(t *testing.T) {
	svc, _, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{}
	svc.window = newTestWindowController(t, swap, &world{}, &fakePub{})
	svc.ledger.RefreshBrainLease(18432)
	sc.gameActive = true // the game runs, the brain went out (no marker)
	sc.probeBrain = func(context.Context) bool { return false }

	sc.gameKeepalive(ctx)

	if swap.openGameCalls != 0 {
		t.Fatalf("openGameCalls = %d, want 0 (the watchdog owns this state)", swap.openGameCalls)
	}
}

// TestGameKeepaliveConvertsPhotoWindow: a photo window that opened (or
// was left) while the game runs is the game's to have — the keepalive
// converts it to the game window (the script stops the art service).
func TestGameKeepaliveConvertsPhotoWindow(t *testing.T) {
	svc, _, _, sc := newStreamTest(t)
	ctx := context.Background()

	swap := &fakeSwap{}
	swap.openAsPhoto()
	svc.window = newTestWindowController(t, swap, &world{}, &fakePub{})
	svc.ledger.RefreshBrainLease(18432)
	sc.gameActive = true
	sc.probeBrain = func(context.Context) bool { return true }

	sc.gameKeepalive(ctx)

	if swap.openGameCalls != 1 || !swap.open || len(swap.forCaps) != 1 || swap.forCaps[0] != "game" {
		t.Fatalf("convert: openGame=%d open=%v for=%v, want the game window",
			swap.openGameCalls, swap.open, swap.forCaps)
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
