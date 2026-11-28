package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/api"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

// fakeLoop is a loop the tests drive: Step counts itself and, when
// blockCh is set, blocks until the test closes it — a tick that keeps
// running through the drain, like a real one.
type fakeLoop struct {
	mu      sync.Mutex
	steps   int
	blockCh chan struct{}
}

func (f *fakeLoop) Step(context.Context) (bool, error) {
	f.mu.Lock()
	f.steps++
	block := f.blockCh
	f.mu.Unlock()
	if block != nil {
		<-block // a running tick does not stop for the drain.
	}
	return false, nil
}

func (f *fakeLoop) stepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps
}

func (f *fakeLoop) IdleWhileReady() time.Duration { return 0 }
func (f *fakeLoop) Submit(string) error           { return nil }
func (f *fakeLoop) HandleOpDone(string, string, ops.Result) error {
	return nil
}
func (f *fakeLoop) HandleApprovalDecided(string, string) error { return nil }

// blockTool blocks until its context ends: an op that is still running
// when the shutdown deadline comes.
type blockTool struct{}

func (blockTool) Spec() ops.Spec {
	return ops.Spec{
		Name:        "block",
		Description: "blocks until the context ends",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Class:       ops.Long,
	}
}

func (blockTool) Run(ctx context.Context, _ ops.Call, _ func()) ops.Result {
	<-ctx.Done()
	return ops.Result{Status: "cancelled", Error: "context done"}
}

// testDaemon builds a deps run can drive: a real store in a temp dir, an
// empty registry, a 0600 token file ("test-token"), and a listen address
// on a free 127.0.0.1 port. It returns the fake loop so a test can make
// its ticks block.
func testDaemon(t *testing.T, graceSeconds int) (deps, config.Config, string, *fakeLoop) {
	t.Helper()
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := config.Default()
	cfg.Loopd.TokenFile = token
	cfg.Loopd.Listen = addr
	cfg.Loopd.ShutdownGraceSeconds = graceSeconds
	_, reg := loop.NewMetrics()
	fl := &fakeLoop{}
	return deps{
		store:    store,
		runner:   &ops.Runner{Registry: ops.NewRegistry(), Timeouts: map[ops.Class]time.Duration{}},
		broker:   api.NewBroker(16),
		drainer:  api.NewDrainer(),
		redactor: redact.New(nil),
		loop:     fl,
		metrics:  reg,
	}, cfg, addr, fl
}

// waitServing polls /healthz until the daemon answers 200.
func waitServing(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon did not come up")
}

// waitForSteps waits until the fake loop has started n ticks.
func waitForSteps(t *testing.T, fl *fakeLoop, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fl.stepCount() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fl.stepCount() < n {
		t.Fatalf("the scheduler started only %d tick(s), wanted %d", fl.stepCount(), n)
	}
}

// doGet performs a GET and returns the status code and body.
func doGet(t *testing.T, url, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestStartFailsWithBadTokenFile: an unusable token file (missing,
// empty, or group/other readable) fails the start — run returns an error
// before anything is served.
func TestStartFailsWithBadTokenFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"missing": filepath.Join(dir, "no-such-file"),
		"empty":   write("empty", nil, 0o600),
		"loose":   write("loose", []byte("s3cret\n"), 0o644),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			d, cfg, _, _ := testDaemon(t, 1)
			cfg.Loopd.TokenFile = path
			if err := run(context.Background(), cfg, d); err == nil {
				t.Fatalf("run started with the unusable token file %s", path)
			}
		})
	}
}

// TestSchedulerStopsAfterDrain: once the drain has begun the scheduler
// starts no new tick.
func TestSchedulerStopsAfterDrain(t *testing.T) {
	d, cfg, addr, fl := testDaemon(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errs := make(chan error, 1)
	go func() { errs <- run(ctx, cfg, d) }()
	waitServing(t, "http://"+addr)
	waitForSteps(t, fl, 1)

	d.drainer.Begin()
	time.Sleep(500 * time.Millisecond) // more than two poll floors: no new tick may have started
	c1 := fl.stepCount()
	time.Sleep(400 * time.Millisecond)
	if c2 := fl.stepCount(); c2 != c1 {
		t.Fatalf("the scheduler started ticks after the drain: steps %d -> %d", c1, c2)
	}

	cancel()
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestSigtermWaitsForRunningTickThenExits: on SIGTERM (ctx cancel) run
// waits for the running tick to finish before it shuts down, and then
// returns nil (exit 0).
func TestSigtermWaitsForRunningTickThenExits(t *testing.T) {
	d, cfg, addr, fl := testDaemon(t, 60) // ample grace
	fl.blockCh = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errs := make(chan error, 1)
	go func() { errs <- run(ctx, cfg, d) }()
	waitServing(t, "http://"+addr)
	waitForSteps(t, fl, 1) // the tick is running, blocked in Step

	cancel() // SIGTERM
	select {
	case err := <-errs:
		t.Fatalf("run exited before the running tick finished: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(fl.blockCh) // the tick finishes
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not exit after the tick finished")
	}
}

// TestSigtermDeadlineExitsWithOpsStillRunning: when the running tick and
// its op are not done by the grace deadline, run exits 0 anyway and
// names the ops still running in the log line.
func TestSigtermDeadlineExitsWithOpsStillRunning(t *testing.T) {
	var buf bytes.Buffer
	logSetOutput(t, &buf)

	d, cfg, addr, fl := testDaemon(t, 1) // one second of grace
	fl.blockCh = make(chan struct{})
	d.runner = &ops.Runner{
		Registry: ops.NewRegistry(blockTool{}),
		Timeouts: map[ops.Class]time.Duration{}, // Long has no deadline.
		OnDone:   func(string, string, ops.Result) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errs := make(chan error, 1)
	go func() { errs <- run(ctx, cfg, d) }()
	waitServing(t, "http://"+addr)
	if err := d.runner.Start(ops.Call{Name: "block", JobID: "job1", OpID: "op1"}); err != nil {
		t.Fatal(err)
	}

	cancel() // SIGTERM: the tick and the op keep running past the deadline
	start := time.Now()
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("run exited after %v, before the one-second shutdown deadline", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "still running") {
		t.Fatalf("no log line naming the ops still running: %q", out)
	}
	if !strings.Contains(out, "op1") {
		t.Fatalf("the still-running op is not named in the log line: %q", out)
	}
}

// TestAPIServedWithToken: the API is served behind the bearer-token
// guard — /healthz and /metrics pass without a token, every /v1 route
// needs the one the daemon loaded from its token file.
func TestAPIServedWithToken(t *testing.T) {
	d, cfg, addr, _ := testDaemon(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errs := make(chan error, 1)
	go func() { errs <- run(ctx, cfg, d) }()
	base := "http://" + addr
	waitServing(t, base)

	if code, _ := doGet(t, base+"/healthz", ""); code != http.StatusOK {
		t.Fatalf("GET /healthz: got %d, want 200", code)
	}
	if code, _ := doGet(t, base+"/metrics", ""); code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d, want 200", code)
	}
	if code, _ := doGet(t, base+"/v1/state", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/state without a token: got %d, want 401", code)
	}
	code, body := doGet(t, base+"/v1/state", "Bearer test-token")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/state with the token: got %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, `"draining"`) {
		t.Fatalf("GET /v1/state with the token: unexpected body %q", body)
	}

	cancel()
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// logSetOutput points the process logger at buf for the duration of the
// test (the tests of this package run serially, so the shared logger is
// safe to redirect).
func logSetOutput(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}
