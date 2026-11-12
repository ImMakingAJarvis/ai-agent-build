package capworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// waitResult polls a flow queue (consuming, like the woken round does)
// until a Result with the given correlation id arrives or the budget runs
// out.
func waitResult(t *testing.T, c *queue.Conn, flowQ, corr string, budget time.Duration) queue.Result {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		del, ok, err := ch.Get(flowQ, true)
		if err != nil {
			t.Fatalf("get %s: %v", flowQ, err)
		}
		if ok {
			res, derr := queue.DecodeResult(del.Body)
			if derr == nil && res.Status != "" && res.CorrelationID == corr {
				return res
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatalf("no result for %s on %s within %s", corr, flowQ, budget)
	return queue.Result{}
}

// fileHandler is the test capability: it counts its calls and writes one
// artifact file to the request's OutDir.
type fileHandler struct {
	calls int32
}

func (f *fileHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	n := atomic.AddInt32(&f.calls, 1)
	name := filepath.Join(req.OutDir, fmt.Sprintf("out-%d.txt", n))
	if err := os.WriteFile(name, []byte("echo body"), 0o644); err != nil {
		return queue.Result{}, err
	}
	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{name},
		Summary:   "test capability done",
	}, nil
}

// TestWorkerEndToEnd is a live test (RabbitMQ on RABBITMQ_URL): the whole
// contract of the SDK in one pass — the worker declares cap.<cap> and
// consumes it, the handler runs, the idempotency marker is written, the
// Result lands on the ReplyTo flow queue, and the delivery is acked. It
// then republishes the SAME message (a redelivery) and checks invariant 4:
// the handler is not called a second time, and the stored result is
// re-replied so the flow queue still gets its record. Skips when the
// broker is unreachable, so `go test ./...` stays green off this machine.
func TestWorkerEndToEnd(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capw-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr := "corr-" + suffix
	outDir := t.TempDir()
	h := &fileHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{
		Cap:    suffix,
		Name:   "test-worker",
		Model:  "test",
		Conn:   c,
		Handler: h,
	})
	go w.Run(ctx)

	// Declare the queues before publishing: in production the worker holds
	// the cap queue up, and a message routed to a queue that does not exist
	// yet is simply dropped (unroutable).
	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	req := queue.Message{
		ID:            "req-" + suffix,
		Type:          "capability",
		Capability:    suffix,
		CorrelationID: corr,
		ReplyTo:       flowQ,
		OutDir:        outDir,
		WakeJob:       "test-job",
		SchemaVersion: 2,
	}
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	res := waitResult(t, c, flowQ, corr, 20*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("artifacts = %v, want exactly one", res.Artifacts)
	}
	if _, err := os.Stat(res.Artifacts[0]); err != nil {
		t.Errorf("artifact %s missing: %v", res.Artifacts[0], err)
	}
	if res.Worker != "test-worker" || res.Model != "test" {
		t.Errorf("worker/model = %q/%q, want the stamped values", res.Worker, res.Model)
	}
	if _, err := os.ReadFile(queue.DoneMarker(outDir, corr)); err != nil {
		t.Errorf("idempotency marker missing: %v", err)
	}

	// Redelivery (invariant 4): the same message again. The handler must
	// NOT run a second time, and the flow queue must get a re-reply built
	// from the marker.
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("republish: %v", err)
	}
	res2 := waitResult(t, c, flowQ, corr, 20*time.Second)
	if res2.Status != queue.ResultOK {
		t.Errorf("re-reply status = %q, want ok", res2.Status)
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("handler calls = %d after redelivery, want 1 (work was redone)", got)
	}
	if len(res2.Artifacts) != 1 {
		t.Errorf("re-reply artifacts = %v, want the stored one", res2.Artifacts)
	}
}

// TestWorkerDeadlineTimeout is a live test: a request whose deadline passed
// before the worker picked it up is answered with status=timeout (the
// switchboard's timeout wake is the job-side record; this is the
// flow-queue-side record), and the handler never runs.
func TestWorkerDeadlineTimeout(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwt-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr := "corr-" + suffix
	h := &fileHandler{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{Cap: suffix, Name: "test-worker", Model: "test", Conn: c, Handler: h})
	go w.Run(ctx)

	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	dl := time.Now().Add(-time.Minute)
	req := queue.Message{
		ID:            "req-" + suffix,
		Type:          "capability",
		Capability:    suffix,
		CorrelationID: corr,
		ReplyTo:       flowQ,
		OutDir:        t.TempDir(),
		Deadline:      &dl,
		SchemaVersion: 2,
	}
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	res := waitResult(t, c, flowQ, corr, 20*time.Second)
	if res.Status != queue.ResultTimeout {
		t.Errorf("status = %q, want timeout", res.Status)
	}
	if got := atomic.LoadInt32(&h.calls); got != 0 {
		t.Errorf("handler ran %d times on a past-deadline request, want 0", got)
	}
}

// TestWorkerRetriesThenDeadLetters is a live test: a handler that always
// fails burns the shared delivery budget (queue.MaxAttempts) — the failed
// delivery is republished with the bumped x-attempts header on each try,
// and at the budget the message is dead-lettered with reason "rejected"
// (invariant 9: nothing is silently acked).
func TestWorkerRetriesThenDeadLetters(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwr-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr := "corr-" + suffix

	h := failingHandler{err: fmt.Errorf("deliberate failure for the retry test")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{Cap: suffix, Name: "test-worker", Model: "test", Conn: c, Handler: h})
	go w.Run(ctx)

	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	req := queue.Message{
		ID:            "req-" + suffix,
		Type:          "capability",
		Capability:    suffix,
		CorrelationID: corr,
		ReplyTo:       flowQ,
		OutDir:        t.TempDir(),
		SchemaVersion: 2,
	}
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		dead, err := c.ListDead(50)
		if err != nil {
			t.Fatalf("list dead: %v", err)
		}
		for _, d := range dead {
			if d.Queue == capQ {
				if d.Reason != "rejected" {
					t.Errorf("death reason = %q, want rejected", d.Reason)
				}
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("message on %s never dead-lettered within 30s", capQ)
}

// failingHandler always fails, to exercise the retry/dead-letter path.
type failingHandler struct{ err error }

func (f failingHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	return queue.Result{}, f.err
}

// okHandler answers immediately (no artifact).
type okHandler struct{}

func (okHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	return queue.Result{Status: queue.ResultOK, Summary: "ok"}, nil
}

// blockingHandler holds each request for block — the test's way of
// occupying a prefetch=1 worker.
type blockingHandler struct{ block time.Duration }

func (b blockingHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	select {
	case <-time.After(b.block):
	case <-ctx.Done():
		return queue.Result{}, ctx.Err()
	}
	return queue.Result{Status: queue.ResultOK, Summary: "blocked done"}, nil
}

// gateBroker is the lease state machine's fake broker: it grants or
// denies on the test's flag, and its State answers the same way —
// flipping the flag to false is the revocation (the lease is gone from
// the ledger).
type gateBroker struct {
	granted atomic.Bool
}

func (g *gateBroker) Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error) {
	if g.granted.Load() {
		return LeaseDecision{Status: LeaseGranted, LeaseID: "l-gate"}, nil
	}
	return LeaseDecision{Status: LeaseDenied, Reason: "denied by the test"}, nil
}

func (g *gateBroker) Release(ctx context.Context, leaseID string) error { return nil }

func (g *gateBroker) State(ctx context.Context, leaseID string) (LeasePresence, error) {
	if g.granted.Load() {
		return LeaseActive, nil
	}
	return LeaseGone, nil
}

// downBroker is a broker that never answers (resourced is down).
type downBroker struct{}

func (downBroker) Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error) {
	return LeaseDecision{}, errors.New("resourced down (test)")
}

func (downBroker) Release(ctx context.Context, leaseID string) error { return nil }

func (downBroker) State(ctx context.Context, leaseID string) (LeasePresence, error) {
	return LeaseGone, nil
}

// TestWorkerLeaseGate is a live test of the plan §3.6 lease state
// machine: while the worker's lease is denied it must NOT consume (the
// message waits on the durable queue, for a lower-priority placement of
// the same capability); once granted, the worker consumes; a revocation
// mid-delivery drains the in-flight request and then cancels the
// consumer; and a re-grant re-consumes.
func TestWorkerLeaseGate(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwl-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr1 := "corr1-" + suffix
	corr2 := "corr2-" + suffix
	corr3 := "corr3-" + suffix
	g := &gateBroker{} // denied until the test flips it

	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{
		Cap:         suffix,
		Name:        "lease-worker",
		Model:       "test",
		Conn:        c,
		Handler:     blockingHandler{block: 3 * time.Second}, // long deliveries: the revoke lands mid-delivery
		LeaseBroker: g,
		LeaseSpec:   LeaseSpec{Holder: "lease-worker", Capability: suffix, Device: "cpu"},
		LeasePollS:  time.Second,
		Log:         log.New(&logBuf, "", 0),
	})
	go w.Run(ctx)

	// Make the queues exist before publishing: a message routed to a queue
	// that does not exist yet is unroutable and dropped.
	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	publish := func(corr string) {
		t.Helper()
		req := queue.Message{
			ID: "req-" + corr, Type: "capability", Capability: suffix,
			CorrelationID: corr, ReplyTo: flowQ, SchemaVersion: 2,
		}
		if err := c.Publish(req, capQ+".default"); err != nil {
			t.Fatalf("publish %s: %v", corr, err)
		}
	}

	// Phase 1: the lease is denied → the worker must not consume; the
	// message waits on the durable queue.
	publish(corr1)
	time.Sleep(2 * time.Second)
	if d, derr := c.QueueDepth(capQ); derr != nil {
		t.Fatalf("depth: %v", derr)
	} else if d != 1 {
		t.Fatalf("queue depth while the lease is denied = %d, want 1 (the worker must not consume)", d)
	}
	if !strings.Contains(logBuf.String(), "denied") {
		t.Errorf("the deny was not logged: %q", logBuf.String())
	}

	// Phase 2: the lease is granted → the worker consumes and answers.
	g.granted.Store(true)
	res := waitResult(t, c, flowQ, corr1, 30*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if res.Worker != "lease-worker" {
		t.Errorf("worker = %q, want lease-worker", res.Worker)
	}

	// Phase 3: the lease is revoked mid-delivery → the in-flight request
	// drains to completion (not requeued), then the consumer cancels.
	publish(corr2)
	time.Sleep(time.Second) // the worker is now holding corr2 (a 3 s delivery)
	g.granted.Store(false)
	res = waitResult(t, c, flowQ, corr2, 30*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok (the in-flight request drains to completion)", res.Status)
	}
	waitNoConsumers(t, c, capQ, 30*time.Second)

	// Phase 4: the lease is granted again → the worker re-consumes.
	g.granted.Store(true)
	waitConsumers(t, c, capQ, 1, 30*time.Second)
	publish(corr3)
	res = waitResult(t, c, flowQ, corr3, 30*time.Second)
	if res.Worker != "lease-worker" {
		t.Errorf("worker = %q, want lease-worker", res.Worker)
	}
}

// TestWorkerLeaseFailOpen is a live test of the resourced-down
// acceptance: a worker whose broker never answers still consumes — it
// fails open to the pre-lease behavior — and logs the warning.
func TestWorkerLeaseFailOpen(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwf-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr := "corr-" + suffix

	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{
		Cap:         suffix,
		Name:        "failopen-worker",
		Model:       "test",
		Conn:        c,
		Handler:     okHandler{},
		LeaseBroker: downBroker{},
		LeaseSpec:   LeaseSpec{Holder: "failopen-worker", Capability: suffix, Device: "cpu"},
		LeasePollS:  time.Second,
		Log:         log.New(&logBuf, "", 0),
	})
	go w.Run(ctx)

	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	req := queue.Message{
		ID: "req-" + suffix, Type: "capability", Capability: suffix,
		CorrelationID: corr, ReplyTo: flowQ, SchemaVersion: 2,
	}
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	res := waitResult(t, c, flowQ, corr, 30*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok (resourced down must not idle the worker)", res.Status)
	}
	if res.Worker != "failopen-worker" {
		t.Errorf("worker = %q, want failopen-worker", res.Worker)
	}
	if !strings.Contains(logBuf.String(), "failing open") {
		t.Errorf("the fail-open warning was not logged: %q", logBuf.String())
	}
}

// TestWorkerLeaseResourced is a live test of the plan §3.6 acceptance
// against a fake resourced (the real HTTP API over a real ledger): with
// the brain's standing lease holding all the VRAM, the gpu worker's
// lease is queued — it holds no lease and must not consume — while the
// cpu worker's RAM lease is granted and it answers the same queue; when
// the brain lease leaves the ledger (the swap window) the gpu worker's
// queued lease is granted and it re-consumes, its priority-10 consumer
// taking the next request.
func TestWorkerLeaseResourced(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capws-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr1 := "corr1-" + suffix
	corr2 := "corr2-" + suffix

	// The box: an 8 GB GPU and 128 GB of RAM, the brain's standing lease
	// holding all the VRAM (the conversation case).
	ledger := resources.New(
		resources.Device{Name: resources.DeviceGPU, Present: true, VRAMTotalMB: 8192},
		resources.Device{Name: resources.DeviceCPU, Present: true, RAMTotalMB: 128 * 1024},
	)
	if _, created := ledger.RefreshBrainLease(8192); !created {
		t.Fatal("the brain's standing lease must be created")
	}
	srv := httptest.NewServer(newFakeResourced(ledger))
	defer srv.Close() // last defer to run: after the workers have shut down
	broker := NewResourcedBroker(srv.URL)

	gpuDone := make(chan struct{})
	cpuDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	// Join the workers after cancel and before srv.Close(): their
	// release-on-exit must reach the fake while it is still serving.
	defer func() { <-gpuDone; <-cpuDone }()
	defer cancel()
	gpuW := New(Config{
		Cap:         suffix,
		Name:        "lease-gpu-worker",
		Model:       "test",
		Location:    "gpu", // consumer priority 10
		Conn:        c,
		Handler:     okHandler{},
		LeaseBroker: broker,
		LeaseSpec:   LeaseSpec{Holder: "lease-gpu-worker", Capability: suffix, Device: resources.DeviceGPU, VRAMMB: 4096},
		LeasePollS:  time.Second,
	})
	cpuW := New(Config{
		Cap:         suffix,
		Name:        "lease-cpu-worker",
		Model:       "test",
		Location:    "cpu", // consumer priority 1
		Conn:        c,
		Handler:     okHandler{},
		LeaseBroker: broker,
		LeaseSpec:   LeaseSpec{Holder: "lease-cpu-worker", Capability: suffix, Device: resources.DeviceCPU, RAMMB: 1024},
		LeasePollS:  time.Second,
	})
	go func() { defer close(gpuDone); gpuW.Run(ctx) }()
	go func() { defer close(cpuDone); cpuW.Run(ctx) }()

	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}
	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	publish := func(corr string) {
		t.Helper()
		req := queue.Message{
			ID: "req-" + corr, Type: "capability", Capability: suffix,
			CorrelationID: corr, ReplyTo: flowQ, SchemaVersion: 2,
		}
		if err := c.Publish(req, capQ+".default"); err != nil {
			t.Fatalf("publish %s: %v", corr, err)
		}
	}

	// Phase 1: the gpu worker's VRAM lease is queued behind the brain's
	// standing lease — it holds no lease and must not consume. The cpu
	// worker's RAM lease is granted, and it answers the same queue.
	publish(corr1)
	waitConsumers(t, c, capQ, 1, 30*time.Second)
	// Steady state: the gpu worker's lease is queued behind the brain's
	// standing lease (all the VRAM is held, so it can never be granted).
	// The worker's startup is not synchronized with the cpu consumer
	// appearing, so wait for the request to land in the ledger.
	inQueue := false
	for deadline := time.Now().Add(10 * time.Second); !inQueue; {
		for _, q := range ledger.State().Queued {
			if q.Holder == "lease-gpu-worker" {
				inQueue = true
				break
			}
		}
		if inQueue || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !inQueue {
		t.Fatal("the gpu worker's lease must be queued in the ledger (the brain holds all the VRAM)")
	}
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if q, ierr := ch.QueueInspect(capQ); ierr != nil {
		t.Fatalf("inspect: %v", ierr)
	} else if q.Consumers != 1 {
		t.Fatalf("consumers while the gpu lease is queued = %d, want 1 (only the cpu worker)", q.Consumers)
	}
	ch.Close()
	// The result itself is the acceptance: the one message on the queue
	// must have been answered by the cpu worker — the only placement with
	// a lease. (If the gpu worker had consumed it, the worker would be
	// "lease-gpu-worker".)
	res := waitResult(t, c, flowQ, corr1, 30*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if res.Worker != "lease-cpu-worker" {
		t.Errorf("worker = %q, want lease-cpu-worker (the only placement with a lease)", res.Worker)
	}

	// Phase 2: the swap window — the brain's standing lease leaves the
	// ledger. The resourced handler re-evaluates the queue and grants the
	// gpu worker's queued lease; it re-consumes, and its priority-10
	// consumer takes the next request.
	var brainID string
	for _, l := range ledger.State().Leases {
		if l.Holder == resources.BrainHolder {
			brainID = l.ID
		}
	}
	if brainID == "" {
		t.Fatal("the brain's standing lease is not in the ledger")
	}
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/lease/"+brainID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("release the brain lease: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release the brain lease: status %d", resp.StatusCode)
	}

	waitConsumers(t, c, capQ, 2, 30*time.Second)
	publish(corr2)
	res = waitResult(t, c, flowQ, corr2, 30*time.Second)
	if res.Worker != "lease-gpu-worker" {
		t.Errorf("worker = %q, want lease-gpu-worker (priority 10 beats 1 once it has a lease)", res.Worker)
	}
}

// TestWorkerIdleUnload is a live test of the idle-unload cycle: with no
// requests the worker unloads its resources after IdleS, and brings them
// back (Load) before the next request is handled.
func TestWorkerIdleUnload(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwi-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)
	corr := "corr-" + suffix
	var loads, unloads atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{
		Cap:     suffix,
		Name:    "idle-worker",
		Model:   "test",
		Conn:    c,
		Handler: okHandler{},
		IdleS:   time.Second,
		Load:    func(ctx context.Context) error { loads.Add(1); return nil },
		Unload:  func(ctx context.Context) error { unloads.Add(1); return nil },
	})
	go w.Run(ctx)

	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	// Wait for the idle unload (the idle window is measured from
	// startup: the worker never got a request).
	deadline := time.Now().Add(10 * time.Second)
	for unloads.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no idle unload within 10 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := loads.Load(); got != 0 {
		t.Fatalf("Load ran %d times before any request, want 0", got)
	}

	req := queue.Message{
		ID: "req-" + suffix, Type: "capability", Capability: suffix,
		CorrelationID: corr, ReplyTo: flowQ, SchemaVersion: 2,
	}
	if err := c.Publish(req, capQ+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	res := waitResult(t, c, flowQ, corr, 20*time.Second)
	if res.Status != queue.ResultOK {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if got := loads.Load(); got != 1 {
		t.Errorf("Load ran %d times before the reloaded request, want 1", got)
	}
	if got := unloads.Load(); got != 1 {
		t.Errorf("Unload ran %d times, want 1 (a request in flight must not re-unload)", got)
	}
}

// TestWorkerHeartbeat is a live test of the worker.<name>.heartbeat
// events: a subscriber bound to worker.# on the events exchange must
// receive heartbeats at the configured interval.
func TestWorkerHeartbeat(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	events, err := queue.NewEventPublisher(c, "capworker-test")
	if err != nil {
		t.Skipf("events publisher: %v", err)
	}
	defer events.Close()

	suffix := fmt.Sprintf("capwh-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{
		Cap:        suffix,
		Name:       "hb-worker",
		Model:      "test",
		Conn:       c,
		Handler:    okHandler{},
		Events:     events,
		HeartbeatS: 300 * time.Millisecond,
	})
	go w.Run(ctx)
	defer deleteQueue(t, c, capQ)

	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	subQ := "hb-" + suffix
	if _, err := ch.QueueDeclare(subQ, true, false, false, false, nil); err != nil {
		t.Fatalf("declare subscriber: %v", err)
	}
	defer ch.QueueDelete(subQ, false, false, false)
	if err := ch.QueueBind(subQ, "worker.#", queue.EventsExchange, false, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		del, ok, err := ch.Get(subQ, true)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if ok {
			e, derr := queue.DecodeEvent(del.Body)
			if derr != nil {
				continue
			}
			if e.Kind == queue.WorkerKind && e.State == "heartbeat" && e.Subject == "hb-worker" {
				if e.Data["capability"] != suffix {
					t.Errorf("heartbeat data capability = %v, want %s", e.Data["capability"], suffix)
				}
				if p, _ := e.Data["priority"].(float64); p != 1 {
					t.Errorf("heartbeat data priority = %v, want 1 (default location)", e.Data["priority"])
				}
				return
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatal("no worker.*.heartbeat event within 5 s")
}

// TestWorkerConsumerPriority is a live test of the plan §3.5 broker-side
// scheduling: two placements of the same capability (a cpu worker at
// consumer x-priority 1 and a gpu worker at x-priority 10) share the cap
// queue. Consumer priority is a pure ranking — the broker delivers each
// message to the highest-priority FREE consumer — so while both are free
// the gpu placement takes the request, and while the gpu worker is busy
// the next request falls through to the free cpu worker: the best free
// worker, with no scheduler in between.
func TestWorkerConsumerPriority(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	suffix := fmt.Sprintf("capwp-%d", time.Now().UnixNano())
	capQ := queue.CapQueueName(suffix)
	flowQ := queue.FlowQueueName(suffix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cpuW := New(Config{Cap: suffix, Name: "a-cpu", Model: "test", Conn: c, Location: "cpu", Handler: okHandler{}})
	gpuW := New(Config{Cap: suffix, Name: "b-gpu", Model: "test", Conn: c, Location: "gpu", Handler: blockingHandler{block: 2500 * time.Millisecond}})
	go cpuW.Run(ctx)
	go gpuW.Run(ctx)

	if err := c.EnsureFlowQueue(flowQ); err != nil {
		t.Fatalf("ensure flow: %v", err)
	}
	defer deleteQueue(t, c, flowQ)
	defer deleteQueue(t, c, capQ)

	mkReq := func(corr string, prio int) queue.Message {
		return queue.Message{
			ID: "req-" + suffix + "-" + corr, Type: "capability", Capability: suffix,
			CorrelationID: corr, ReplyTo: flowQ, Priority: prio, SchemaVersion: 2,
		}
	}

	// Wait until both workers are actually consuming before publishing.
	// The broker confirms queue.bind on the queue side, but the exchange's
	// routing table catches up asynchronously: a publish that races the
	// workers' startup binds (on another channel of the same connection)
	// comes back basic.return NO_ROUTE and the message is dropped — a
	// synchronous (noWait=false) bind does not close this window. In the
	// running system the workers are long up before a request arrives;
	// this wait is what sets up that state here.
	waitConsumers(t, c, capQ, 2, 10*time.Second)

	// Make the queue exist before publishing (a no-op now that the
	// workers created it, but the publisher-side contract stays): a
	// message routed to a queue that does not exist yet is unroutable
	// and dropped.
	if err := c.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("ensure cap: %v", err)
	}

	// Phase 1: both placements free — the gpu worker (the higher consumer
	// priority) takes the request.
	m1 := mkReq("p1-"+suffix, 5)
	if err := c.Publish(m1, capQ+".default"); err != nil {
		t.Fatalf("publish m1: %v", err)
	}
	res := waitResult(t, c, flowQ, m1.CorrelationID, 20*time.Second)
	if res.Worker != "b-gpu" {
		t.Fatalf("request answered by %q, want b-gpu (the free highest-priority consumer)", res.Worker)
	}

	// Phase 2: occupy the gpu worker (it takes the next request), then the
	// following request must fall through to the free cpu worker.
	m2 := mkReq("p2-"+suffix, 0)
	if err := c.Publish(m2, capQ+".default"); err != nil {
		t.Fatalf("publish m2: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // b-gpu is now blocked in its handler (prefetch 1)
	m3 := mkReq("p3-"+suffix, 1)
	if err := c.Publish(m3, capQ+".default"); err != nil {
		t.Fatalf("publish m3: %v", err)
	}
	res3 := waitResult(t, c, flowQ, m3.CorrelationID, 20*time.Second)
	if res3.Worker != "a-cpu" {
		t.Errorf("request while the gpu worker is busy answered by %q, want a-cpu", res3.Worker)
	}
	res2 := waitResult(t, c, flowQ, m2.CorrelationID, 20*time.Second)
	if res2.Worker != "b-gpu" {
		t.Errorf("occupying request answered by %q, want b-gpu (it was the free highest-priority consumer)", res2.Worker)
	}
}

func deleteQueue(t *testing.T, c *queue.Conn, name string) {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		return
	}
	defer ch.Close()
	ch.QueueDelete(name, false, false, false)
}

// waitConsumers polls the queue until n consumers are attached (the
// workers finished their ensure+consume), failing after the timeout. A
// 404 (queue not declared yet) is just "not yet".
func waitConsumers(t *testing.T, c *queue.Conn, name string, n int, timeout time.Duration) {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("wait consumers channel: %v", err)
	}
	defer ch.Close()
	deadline := time.Now().Add(timeout)
	last := -1
	for time.Now().Before(deadline) {
		if q, ierr := ch.QueueInspect(name); ierr == nil {
			last = q.Consumers
			if q.Consumers >= n {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("queue %s: %d consumers not attached within %v (last seen: %d)", name, n, timeout, last)
}

// waitNoConsumers polls the queue until it has no consumers (the worker
// cancelled), failing after the timeout. A 404 (queue deleted) is fine.
func waitNoConsumers(t *testing.T, c *queue.Conn, name string, timeout time.Duration) {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	deadline := time.Now().Add(timeout)
	last := -1
	for time.Now().Before(deadline) {
		if q, ierr := ch.QueueInspect(name); ierr == nil {
			last = q.Consumers
			if q.Consumers == 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("queue %s still has consumers after %v (last seen: %d)", name, timeout, last)
}
