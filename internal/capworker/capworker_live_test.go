package capworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
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

func deleteQueue(t *testing.T, c *queue.Conn, name string) {
	t.Helper()
	ch, err := c.NewChannel()
	if err != nil {
		return
	}
	defer ch.Close()
	ch.QueueDelete(name, false, false, false)
}
