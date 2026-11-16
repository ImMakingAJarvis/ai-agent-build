package main

// Live tests (RabbitMQ on RABBITMQ_URL, default localhost:5672) of
// `jarvis-mq call` — the synchronous RPC over direct reply-to (plan
// §3.8, EVB-38). The worker half is the real capworker SDK running
// in-process on a scratch cap queue (cap.echotest), so the full path is
// under test: the CLI's consume-before-publish, the broker rewriting the
// delivered request's reply-to property to a private per-caller queue,
// the SDK replying to that property (not the JSON body), and the
// correlation match. Skips when the broker is unreachable, so
// `go test ./...` stays green off this machine.

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// callTestCap is a scratch capability: its queue (cap.echotest) belongs
// to these tests, no production worker consumes it.
const callTestCap = "echotest"

// sleepHandler answers after d (0 = immediately), echoing the request's
// input into the Result's data.
type sleepHandler struct{ d time.Duration }

func (h sleepHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	if h.d > 0 {
		select {
		case <-ctx.Done():
			return queue.Result{}, ctx.Err()
		case <-time.After(h.d):
		}
	}
	return queue.Result{
		Status:  queue.ResultOK,
		Summary: "call live test: " + req.CorrelationID,
		Data:    map[string]any{"input": req.Input},
	}, nil
}

// startCallTestWorker runs one real SDK worker in-process on cap.echotest
// with the handler's delay, and stops it at test end.
func startCallTestWorker(t *testing.T, conn *queue.Conn, delay time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := capworker.New(capworker.Config{
		Cap:        callTestCap,
		Name:       callTestCap + "@cpu/call-live-test",
		Model:      "echo",
		Location:   "cpu",
		Conn:       conn,
		Handler:    sleepHandler{delay},
		OutDirBase: t.TempDir(),
		Log:        log.New(io.Discard, "", 0),
	})
	go w.Run(ctx)
	t.Cleanup(cancel)
}

// openCallTestBroker connects to the test broker, skips the live tests
// when it is unreachable, and clears the scratch queue (a crashed run
// must not leave a message ahead of the test's own request).
func openCallTestBroker(t *testing.T) *queue.Conn {
	t.Helper()
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	t.Cleanup(c.Close)
	qname := queue.CapQueueName(callTestCap)
	if err := c.EnsureCapQueue(qname); err != nil {
		t.Fatalf("ensure %s: %v", qname, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueuePurge(qname, false); err != nil {
		t.Fatalf("purge %s: %v", qname, err)
	}
	t.Cleanup(func() {
		ch, err := c.NewChannel()
		if err != nil {
			return
		}
		defer ch.Close()
		ch.QueueDelete(qname, false, false, false)
	})
	return c
}

// TestCallRoundTrip is the acceptance round-trip: runCall publishes the
// request with reply-to = amq.rabbitmq.reply-to and must come back with
// the worker's Result (status ok, the input echoed), exit-clean.
func TestCallRoundTrip(t *testing.T) {
	c := openCallTestBroker(t)
	startCallTestWorker(t, c, 100*time.Millisecond)

	err := runCall([]string{callTestCap, "--corr", "call-rt-1", "--input", "msg=hi", "--timeout", "30s"})
	if err != nil {
		t.Fatalf("runCall: %v", err)
	}
}

// TestCallTimeoutAndAsyncSurvival pins the timeout contract: the worker
// is slower than the call's --timeout, so runCall must fail with the
// timeout error — and the request must STAY on the cap queue for the
// async path: the worker still processes it (its idempotency marker
// appears), its Result being dropped because the private reply queue is
// gone (at-most-once).
func TestCallTimeoutAndAsyncSurvival(t *testing.T) {
	c := openCallTestBroker(t)
	outBase := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := capworker.New(capworker.Config{
		Cap:        callTestCap,
		Name:       callTestCap + "@cpu/call-live-test",
		Model:      "echo",
		Location:   "cpu",
		Conn:       c,
		Handler:    sleepHandler{d: 5 * time.Second},
		OutDirBase: outBase,
		Log:        log.New(io.Discard, "", 0),
	})
	go w.Run(ctx)

	const corr = "call-timeout-1"
	err := runCall([]string{callTestCap, "--corr", corr, "--timeout", "1s"})
	if err == nil {
		t.Fatal("runCall: expected the timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %q, want it to contain %q", err, "timed out")
	}

	// The async path: the worker still did the work. Its marker
	// (written before the reply, invariant 3) is the proof.
	marker := queue.DoneMarker(filepath.Join(outBase, "cap-"+callTestCap, corr), corr)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // work done: the request survived the caller's timeout
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("worker did not process the timed-out request: marker %s never appeared", marker)
}
