package main

// The window controller's lifecycle against fakes (task EVB-27,
// acceptance 1): queued GPU work + idle brain → window opens → the
// work drains → the window closes after min_window → and the
// brain-wait / rule-3 / stale / failure paths. No broker, no store on
// disk (except the localJobs test), no jarvis-swap, no 4090.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// fakeSwap records the controller's swap actions and serves a state.
type fakeSwap struct {
	mu         sync.Mutex
	open       bool
	forCaps    []string
	openedAt   time.Time
	openCalls  int
	closeCalls int
	lastOpen   string // "caps | reason"
	lastClose  string
	failOpen   error
	failClose  error
}

func (f *fakeSwap) Open(ctx context.Context, forCaps []string, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	if f.failOpen != nil {
		return f.failOpen
	}
	f.open = true
	f.forCaps = append([]string(nil), forCaps...)
	f.openedAt = time.Now()
	f.lastOpen = strings.Join(forCaps, ",") + " | " + reason
	return nil
}

func (f *fakeSwap) Close(ctx context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	if f.failClose != nil {
		return f.failClose
	}
	f.open = false
	f.forCaps = nil
	f.openedAt = time.Time{}
	f.lastClose = reason
	return nil
}

func (f *fakeSwap) State() (bool, []string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open, f.forCaps, f.openedAt, nil
}

// backdate ages the open window by d (the controller uses the wall
// clock; the tests move the window's opened_at instead).
func (f *fakeSwap) backdate(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openedAt = f.openedAt.Add(-d)
}

// world is the tick's external facts: queue depths, the management
// API's unacked counts and the local jobs.
type world struct {
	mu       sync.Mutex
	depth    map[string]int // queue name → ready
	unacked  map[string]int // queue name → unacked
	depthErr error
	uaErr    error
	jobsErr  error
	running  int
	ready    []resources.ReadyJob
}

func (w *world) setDepth(m map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.depth = m
}

func (w *world) setUnacked(m map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.unacked = m
}

// newTestWindowController wires a controller over the world with the
// plan-default config and two watched capabilities.
func newTestWindowController(t *testing.T, swap *fakeSwap, w *world, pub Publisher) *windowController {
	t.Helper()
	return newWindowController(
		resources.DefaultWindowConfig(),
		swap,
		[]string{"image.generate", "video.render"},
		func(ctx context.Context, name string) (int, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.depthErr != nil {
				return 0, w.depthErr
			}
			return w.depth[name], nil
		},
		func(ctx context.Context) (map[string]int, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.uaErr != nil {
				return nil, w.uaErr
			}
			out := make(map[string]int, len(w.unacked))
			for k, v := range w.unacked {
				out[k] = v
			}
			return out, nil
		},
		func(ctx context.Context) (int, []resources.ReadyJob, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.jobsErr != nil {
				return 0, nil, w.jobsErr
			}
			return w.running, w.ready, nil
		},
		pub,
		prometheus.NewRegistry(),
		slog.New(slog.DiscardHandler),
	)
}

// TestWindowLifecycle is the acceptance path: queued GPU work and an
// idle brain open the window for exactly the capability with work;
// inside the window the drain is watched (ready AND unacked); a
// drained window younger than min_window holds (no thrash); an older
// drained window closes, with the window.opened / window.closed events
// in between.
func TestWindowLifecycle(t *testing.T) {
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 2}}
	pub := &fakePub{}
	wc := newTestWindowController(t, swap, w, pub)
	ctx := context.Background()

	// 1) Work queued, brain idle → the window opens for the one
	//    capability with work (not the empty one).
	wc.tick(ctx)
	if !swap.open || swap.openCalls != 1 || len(swap.forCaps) != 1 || swap.forCaps[0] != "image.generate" {
		t.Fatalf("first tick: open=%v calls=%d for=%v, want open for [image.generate]",
			swap.open, swap.openCalls, swap.forCaps)
	}
	if got := pub.keys(); !lastIs(got, "window.opened") {
		t.Fatalf("events = %v, want last window.opened", got)
	}

	// 2) Inside the window, work still queued → hold (drain, don't
	//    close): the second tick must not close and must not reopen.
	wc.tick(ctx)
	if !swap.open || swap.closeCalls != 0 || swap.openCalls != 1 {
		t.Fatalf("work queued: open=%v closes=%d opens=%d", swap.open, swap.closeCalls, swap.openCalls)
	}

	// 3) The work finishes (ready zero) but the window is younger than
	//    min_window → hold (no thrash).
	w.setDepth(map[string]int{})
	wc.tick(ctx)
	if !swap.open || swap.closeCalls != 0 {
		t.Fatalf("drained but young: open=%v closes=%d, want still open", swap.open, swap.closeCalls)
	}

	// 4) The window passes min_window, still drained → close.
	swap.backdate(3 * time.Minute)
	wc.tick(ctx)
	if swap.open || swap.closeCalls != 1 {
		t.Fatalf("drained + old: open=%v closes=%d, want closed", swap.open, swap.closeCalls)
	}
	if swap.lastClose != "drained (all capability queues empty)" {
		t.Fatalf("close reason = %q", swap.lastClose)
	}
	if got := pub.keys(); !lastIs(got, "window.closed") {
		t.Fatalf("events = %v, want last window.closed", got)
	}
}

// TestWindowUnackedKeepsWindowOpen: the in-flight message the worker
// holds unacked is work — with the management API up it counts, and
// when the API is down the controller falls back to ready counts
// (which here still say "not drained", the safe direction).
func TestWindowUnackedKeepsWindowOpen(t *testing.T) {
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	wc := newTestWindowController(t, swap, w, &fakePub{})
	ctx := context.Background()

	wc.tick(ctx) // open
	if !swap.open {
		t.Fatalf("setup: the window should open")
	}
	// The job confirms: ready drops to zero while the worker still
	// holds the delivery unacked.
	w.setDepth(map[string]int{})
	w.setUnacked(map[string]int{queue.CapQueueName("image.generate"): 1})
	swap.backdate(10 * time.Minute)
	wc.tick(ctx)
	if !swap.open || swap.closeCalls != 0 {
		t.Fatalf("in-flight work unacked: open=%v closes=%d, want still open", swap.open, swap.closeCalls)
	}
	// The Result is confirmed (unacked zero): drained and old → close.
	w.setUnacked(map[string]int{})
	wc.tick(ctx)
	if swap.open || swap.closeCalls != 1 {
		t.Fatalf("confirmed: open=%v closes=%d, want closed", swap.open, swap.closeCalls)
	}

	// With the management API down the fallback is ready-only: a zero
	// ready count would look drained even with an in-flight delivery,
	// so the tick must hold rather than close on a guess.
	swap2 := &fakeSwap{}
	w2 := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}, uaErr: errors.New("mgmt down")}
	wc2 := newTestWindowController(t, swap2, w2, &fakePub{})
	wc2.tick(ctx) // open (ready 1)
	w2.setDepth(map[string]int{})
	swap2.backdate(10 * time.Minute)
	wc2.tick(ctx)
	if !swap2.open {
		t.Fatalf("mgmt down + ready-only: the in-flight work must keep the window open")
	}
}

// TestWindowRule3: a running local round blocks the open; when it
// finishes, the next tick opens (the queued work is still there).
func TestWindowRule3(t *testing.T) {
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 4}, running: 1}
	wc := newTestWindowController(t, swap, w, &fakePub{})
	ctx := context.Background()

	wc.tick(ctx)
	if swap.open || swap.openCalls != 0 {
		t.Fatalf("rule 3: a running local round must block the open (open=%v)", swap.open)
	}
	w.mu.Lock()
	w.running = 0
	w.mu.Unlock()
	wc.tick(ctx)
	if !swap.open || swap.openCalls != 1 {
		t.Fatalf("after the round ends: open=%v calls=%d, want open", swap.open, swap.openCalls)
	}
}

// TestWindowBrainWaitCloses: a ready brain job that has waited past
// max_brain_wait closes the window even with work still queued — the
// job resumes the moment the brain is back.
func TestWindowBrainWaitCloses(t *testing.T) {
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	wc := newTestWindowController(t, swap, w, &fakePub{})
	ctx := context.Background()

	wc.tick(ctx) // open (a ready job does not block the open — only running does)
	if !swap.open {
		t.Fatalf("setup: the window should open")
	}
	now := time.Now()
	w.mu.Lock()
	w.ready = []resources.ReadyJob{{ID: "brain-42", ReadySince: now.Add(-21 * time.Minute)}}
	w.mu.Unlock()
	swap.backdate(30 * time.Minute)
	wc.tick(ctx)
	if swap.open || swap.closeCalls != 1 {
		t.Fatalf("brain wait: open=%v closes=%d, want closed", swap.open, swap.closeCalls)
	}
	if !strings.HasPrefix(swap.lastClose, "brain job brain-42") {
		t.Fatalf("close reason = %q, want the brain-wait reason", swap.lastClose)
	}

	// And under max_brain_wait the window holds (the job is young).
	swap2 := &fakeSwap{}
	w2 := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	wc2 := newTestWindowController(t, swap2, w2, &fakePub{})
	wc2.tick(ctx)
	w2.mu.Lock()
	w2.ready = []resources.ReadyJob{{ID: "brain-7", ReadySince: now.Add(-5 * time.Minute)}}
	w2.mu.Unlock()
	swap2.backdate(30 * time.Minute)
	wc2.tick(ctx)
	if !swap2.open || swap2.closeCalls != 0 {
		t.Fatalf("young brain job: open=%v closes=%d, want still open", swap2.open, swap2.closeCalls)
	}
}

// TestWindowStaleMarker: a window state file without an opened_at
// (hand-written, truncated, or from before EVB-27) is closed on the
// first tick so the controller takes over from a known state.
func TestWindowStaleMarker(t *testing.T) {
	swap := &fakeSwap{}
	swap.mu.Lock()
	swap.open = true // no opened_at
	swap.mu.Unlock()
	wc := newTestWindowController(t, swap, &world{}, &fakePub{})
	wc.tick(context.Background())
	if swap.open || swap.closeCalls != 1 {
		t.Fatalf("stale marker: open=%v closes=%d, want closed", swap.open, swap.closeCalls)
	}
}

// TestWindowSwapFailuresRetry: a failed open or close publishes no
// event and is retried on the next tick (the decision is recomputed
// from the world each tick, so the retry needs no state of its own).
func TestWindowSwapFailuresRetry(t *testing.T) {
	swap := &fakeSwap{failOpen: errors.New("docker exploded")}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	pub := &fakePub{}
	wc := newTestWindowController(t, swap, w, pub)
	ctx := context.Background()

	wc.tick(ctx)
	if swap.openCalls != 1 || swap.open {
		t.Fatalf("failed open: calls=%d open=%v", swap.openCalls, swap.open)
	}
	if len(pub.keys()) != 0 {
		t.Fatalf("a failed open must not publish: %v", pub.keys())
	}
	swap.mu.Lock()
	swap.failOpen = nil
	swap.mu.Unlock()
	wc.tick(ctx)
	if !swap.open {
		t.Fatalf("retry: the window should open")
	}
	if got := pub.keys(); !lastIs(got, "window.opened") {
		t.Fatalf("events after retry = %v, want window.opened", got)
	}

	// The same for a failed close: drained + old, close fails, still
	// open; next tick closes for real.
	w.setDepth(map[string]int{})
	swap.backdate(10 * time.Minute)
	swap.mu.Lock()
	swap.failClose = errors.New("brain start hung")
	swap.mu.Unlock()
	wc.tick(ctx)
	if swap.closeCalls != 1 || !swap.open {
		t.Fatalf("failed close: calls=%d open=%v", swap.closeCalls, swap.open)
	}
	swap.mu.Lock()
	swap.failClose = nil
	swap.mu.Unlock()
	wc.tick(ctx)
	if swap.open || swap.closeCalls != 2 {
		t.Fatalf("close retry: open=%v calls=%d", swap.open, swap.closeCalls)
	}
}

// TestMgmtBase derives the management API base from the amqp URL (same
// host and credentials, port 15672) and rejects a URL with no host.
func TestMgmtBase(t *testing.T) {
	base, user, err := mgmtBase("amqp://jarvis:jarvis-local@rabbitmq:5672")
	if err != nil || base != "http://rabbitmq:15672" {
		t.Fatalf("mgmtBase = %q, %v; want http://rabbitmq:15672", base, err)
	}
	if user == nil || user.Username() != "jarvis" {
		t.Fatalf("user = %v, want jarvis", user)
	}
	pass, _ := user.Password()
	if pass != "jarvis-local" {
		t.Fatalf("password = %q", pass)
	}
	if _, _, err := mgmtBase("amqp://:5672"); err == nil {
		t.Fatalf("a URL without a host must be an error")
	}
}

// writeJob puts one job in the store through the real store API and
// backdates its state file (the ready-since localJobs measures).
func writeJob(t *testing.T, store *agentjob.Store, id, driver, status string, age time.Duration) {
	t.Helper()
	if _, err := store.Ingest(agentjob.Spec{ID: id, Driver: driver}); err != nil {
		t.Fatalf("ingest %s: %v", id, err)
	}
	if err := store.SetStatus(id, agentjob.State{Status: status}); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	if age > 0 {
		p := filepath.Join(store.JobDir(id), "state.json")
		past := time.Now().Add(-age)
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", id, err)
		}
	}
}

// writePhotoJob is writeJob with the photo tag (the executor is code —
// jarvis-photo — so the job is GPU work, not a brain round).
func writePhotoJob(t *testing.T, store *agentjob.Store, id, status string, age time.Duration) {
	t.Helper()
	spec := agentjob.Spec{ID: id, Tags: []string{agentjob.TagPhoto}}
	if _, err := store.Ingest(spec); err != nil {
		t.Fatalf("ingest %s: %v", id, err)
	}
	if err := store.SetStatus(id, agentjob.State{Status: status}); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	if age > 0 {
		p := filepath.Join(store.JobDir(id), "state.json")
		past := time.Now().Add(-age)
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", id, err)
		}
	}
}

// TestLocalJobs counts the local (brain) jobs a tick sees: running and
// ready local ones, not remote-driver or waiting ones, with the state
// file's mtime as the ready time. Photo-tagged jobs are excluded even
// with a local driver — the running one must not block rule 3 (that
// deadlocks the window the job itself waits for) and the ready one's
// 30m wait must not trigger the brain-wait close.
func TestLocalJobs(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	writeJob(t, store, "run-1", "", agentjob.Running, 0)
	writeJob(t, store, "ready-1", "", agentjob.Ready, 21*time.Minute)
	writeJob(t, store, "ready-remote", "sonnet", agentjob.Ready, 21*time.Minute)
	writeJob(t, store, "wait-1", "", agentjob.Waiting, 0)
	writeJob(t, store, "igpu-1", "igpu", agentjob.Running, 0) // not a brain job
	writePhotoJob(t, store, "photo-run", agentjob.Running, 0)
	writePhotoJob(t, store, "photo-ready", agentjob.Ready, 30*time.Minute)

	running, ready, err := localJobs(store, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("localJobs: %v", err)
	}
	if running != 1 {
		t.Fatalf("running = %d, want 1 (a running photo job is GPU work, not a brain round)", running)
	}
	if len(ready) != 1 || ready[0].ID != "ready-1" {
		t.Fatalf("ready = %+v, want only ready-1", ready)
	}
	waited := time.Since(ready[0].ReadySince)
	if waited < 20*time.Minute || waited > 25*time.Minute {
		t.Fatalf("ready-1 waited %s, want ~21m", waited)
	}
}

// TestStateIncludesWindow: GET /state keeps the broker's top-level
// keys (devices/leases/queued — capworker parses them) and adds the
// window as an extra field.
func TestStateIncludesWindow(t *testing.T) {
	svc, _, _ := newTestService(t)
	swap := &fakeSwap{}
	w := &world{depth: map[string]int{queue.CapQueueName("image.generate"): 1}}
	pub := &fakePub{}
	svc.window = newTestWindowController(t, swap, w, pub)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", svc.handleState)

	code, st := doWith(t, mux, "GET", "/state", "")
	if code != 200 {
		t.Fatalf("state: %d", code)
	}
	for _, k := range []string{"devices", "leases", "queued", "window"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("state missing top-level key %q: %v", k, st)
		}
	}
	winfo := st["window"].(map[string]any)
	if winfo["open"] != false {
		t.Fatalf("window before any tick = %v, want open=false", winfo["open"])
	}
}
