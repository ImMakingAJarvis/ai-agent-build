// cap-echo is the test capability worker behind the event-driven brain's
// Phase 2 gate (plan task 17): a fake capability that proves the whole
// wait/wake loop — submit, yield, wake, resume — before any real model
// worker exists. It does the smallest honest thing a capability does:
// wait a configurable delay (30 s by default), write an artifact file to
// the request's OutDir, and reply with a Result whose artifacts point at
// the file (a reference, not the bytes — invariant 7).
//
// It runs on the capworker SDK with the SDK's full placement story
// (plan §3.5): location cpu (consumer priority 1 — the Phase 2 gate's
// submit publishes at priority 1, so a cpu worker takes it), a heartbeat
// on the events exchange, and the metrics endpoint behind
// ECHO_METRICS_ADDR (off by default — the gate does not scrape it).
//
// Configuration (env):
//
//	RABBITMQ_URL         the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	ECHO_DELAY           how long one echo takes. Default 30s.
//	ECHO_OUT_DIR         artifact root for requests without an OutDir.
//	                     Default ~/.local/share/jarvis/artifacts.
//	ECHO_METRICS_ADDR    serve Prometheus /metrics here (e.g. 127.0.0.1:9115).
//	                     Default: off.
//	RESOURCED_URL        the resourced lease broker (plan §3.6); the
//	                     worker gates its consuming on a lease from it.
//	                     Default: off (un-gated, the pre-lease behavior).
//	CAPABILITIES_FILE    the capability catalog the lease's memory spec
//	                     comes from. Needed when RESOURCED_URL is set.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	capability = "echo"
	workerName = "echo@cpu/cap-echo"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key, def string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		d, _ := time.ParseDuration(def)
		return d
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("cap-echo: bad %s %q", key, v)
	}
	return d
}

// connectWithRetry dials the broker, backing off until it is reachable
// (the compose stack may bring rabbitmq up a little after cap-echo).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("cap-echo: broker not ready: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// handler is the echo capability: sleep, write the echo file, reply. The
// file carries the correlation id and the timestamp — the woken round has
// something concrete to reference, and a re-run is visible (the mtime
// changes) if the idempotency marker is ever bypassed.
type handler struct {
	delay time.Duration
}

func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	log.Printf("cap-echo: %s: echoing (delay %s)", req.CorrelationID, h.delay)
	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		// Worker shutting down: do not claim the request — return an error
		// so the SDK requeues the redelivery for a live worker.
		return queue.Result{}, ctx.Err()
	}
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}
	name := filepath.Join(req.OutDir, "echo-"+req.CorrelationID+".txt")
	body := fmt.Sprintf("cap.echo test artifact\ncorrelation: %s\ncapability: %s\njob: %s\nworker: %s\ngenerated: %s\n",
		req.CorrelationID, req.Capability, req.WakeJob, workerName, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", name, err)
	}
	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{name},
		Summary:   fmt.Sprintf("echoed after %s", h.delay),
		Data:      map[string]any{"delay": h.delay.String()},
	}, nil
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cap-echo: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("cap-echo: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "cap-echo")
	if err != nil {
		log.Printf("cap-echo: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	// Lease gating (plan §3.6): resourced decides whether this placement
	// may consume (the lease carries the catalog's cpu RAM footprint).
	// No broker (RESOURCED_URL unset) or a catalog problem = un-gated,
	// the pre-lease behavior.
	leaseBroker, leaseSpec, lerr := capworker.LeaseFor(
		os.Getenv("RESOURCED_URL"), os.Getenv("CAPABILITIES_FILE"),
		capability, "cpu", workerName)
	if lerr != nil {
		log.Printf("cap-echo: lease: %v (running un-gated)", lerr)
	}

	w := capworker.New(capworker.Config{
		Cap:         capability,
		Name:        workerName,
		Model:       "echo",
		Location:    "cpu", // consumer priority 1 (LocationPriority); no behavior change — the gate submits at priority 1
		Conn:        conn,
		Events:      events,
		LeaseBroker: leaseBroker,
		LeaseSpec:   leaseSpec,
		MetricsAddr: os.Getenv("ECHO_METRICS_ADDR"),
		Handler:     handler{delay: envDuration("ECHO_DELAY", "30s")},
		OutDirBase:  envOr("ECHO_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:         log.Default(),
	})
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("cap-echo: run: %v", err)
	}
	log.Printf("cap-echo: stopped")
}
