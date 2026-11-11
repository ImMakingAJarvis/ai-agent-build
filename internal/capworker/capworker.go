// Package capworker is the capability-worker SDK (plan §3.5). A worker
// names a capability ("echo" → cap.echo), implements a Handler, and runs:
// the SDK declares the queue (durable, x-max-priority 10, dead-letter
// args — idempotently, on every start), consumes with prefetch=1, and
// owns the envelope's contract for every delivery:
//
//   - idempotent by correlation_id (invariant 4): before doing work it
//     checks <out_dir>/.done-<corr>; a redelivery republishes the stored
//     Result and acks — the work is never redone;
//   - results are published to the request's ReplyTo in confirm mode and
//     the delivery is acked only after the broker confirms the result
//     (invariant 3: nothing is lost, even across a crash between work and
//     reply — the marker survives, the redelivery re-replies);
//   - a handler failure burns one of the shared delivery budget
//     (queue.MaxAttempts, x-attempts header): the failed delivery is
//     republished with BumpAttempt after the broker confirms the copy,
//     and at the budget the message is dead-lettered with a reason
//     (invariant 9: processed, dead-lettered, or requeued — never a
//     silent ack);
//   - a request whose deadline passed before pickup is answered with a
//     Result of status=timeout, so the flow queue keeps a consistent
//     record even when the switchboard's timeout wake already fired.
//
// On top of the contract, the SDK places the worker in the broker's
// scheduling:
//
//   - consumer priority per location (LocationPriority): the consumer
//     declares x-priority GPU 10 / iGPU 5 / CPU 1, and RabbitMQ delivers
//     each message to the highest-priority FREE consumer that can take
//     it — the best free worker, with no scheduler in between (plan
//     §3.5). A capability that may run in several places runs one worker
//     per placement; while the GPU worker is busy or has no lease, its
//     messages fall to the cheaper placement.
//   - a resourced lease (plan §3.6, lease.go): before every consume
//     session the worker requests a lease from the resourced broker
//     (LeaseBroker) — holder the worker identity, capability, device its
//     location, memory the catalog's entry for the placement. No lease
//     means it does not consume (its messages stay on the queue for a
//     lower-priority placement — the fallback emerges from the consumer
//     priorities, no router); a grant while it waits means it
//     re-consumes; a lost lease (revocation) drains the in-flight
//     delivery, then cancels the consumer. When resourced is unreachable
//     the worker fails open to the pre-lease behavior (assumes granted,
//     warning logged) and re-syncs on its poll tick. No broker
//     (LeaseBroker nil) = un-gated.
//   - idle unload: after IdleS without a request the worker calls Unload
//     to free its resources (model weights, a GPU process) and calls
//     Load again before the next request it has to handle.
//
// The SDK also emits the job.<capability>.done / .failed lifecycle
// events plus worker.<name>.heartbeat / .loaded / .unloaded events on
// the internal/queue events exchange, and serves Prometheus /metrics
// (deliveries, handler duration, inflight, loaded, last work) when
// MetricsAddr is set — so other services can watch capability work
// without reading the flow queues.
package capworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// resultConfirmTimeout bounds how long a reply waits for the broker to
// confirm the Result before giving up (the same bar as the switchboard's
// forwards).
const resultConfirmTimeout = 10 * time.Second

// Handler does the work of one capability request and returns the Result
// (status, artifacts, summary; the SDK stamps the envelope fields). The
// request's OutDir is already filled in — from the envelope, or the
// worker's default — so the handler writes its artifacts there. Return an
// error to burn an attempt (see the package comment); return a Result to
// finish. The handler must not publish the Result itself: the SDK does,
// in confirm mode, after writing the idempotency marker.
type Handler interface {
	Handle(ctx context.Context, req queue.Message) (queue.Result, error)
}

// Config wires one capability worker.
type Config struct {
	// Cap is the capability, e.g. "echo" (queue cap.echo, events
	// job.echo.*). Required.
	Cap string
	// Name is the worker identity recorded on Results, e.g.
	// "echo@cpu/cap-echo".
	Name string
	// Model is the model that produced the results ("echo" for the test
	// worker).
	Model string
	// Location is where this worker runs: "gpu", "igpu" or "cpu" (the
	// capability catalog's vocabulary; an off-box "remote:<host>" worker
	// takes the CPU priority). It sets the consumer's AMQP priority
	// (LocationPriority), so the broker delivers each request to the
	// best free worker — GPU 10, iGPU 5, CPU 1 (plan §3.5). Empty = cpu.
	Location string
	// Priority overrides the location-derived consumer priority; 0 =
	// derive it from Location.
	Priority int
	// LeaseBroker gates this worker's consuming on a resourced lease
	// (plan §3.6): the worker requests the spec's lease before every
	// consume session and holds it while it consumes — released on idle
	// unload (the resources are free again) and on shutdown. No lease
	// means no consuming: the messages fall to a lower-priority
	// placement of the same capability. A queued lease is re-checked on
	// the poll interval; when it is granted the worker re-consumes. A
	// lost lease (revoked by the window controller) drains the in-flight
	// delivery, then cancels the consumer. When the broker is unreachable
	// the worker fails open (assumes granted, warning logged) and
	// re-syncs on the poll tick. nil = un-gated (the pre-lease behavior).
	LeaseBroker LeaseBroker
	// LeaseSpec is what the worker requests from the broker: holder (the
	// worker identity), capability, device (its location) and the memory
	// footprints — the capability catalog's entry for the placement
	// (LeaseFor builds it). Used only when LeaseBroker is set.
	LeaseSpec LeaseSpec
	// LeasePollS is the lease state machine's poll interval: re-ask a
	// denied lease, check a queued one for its grant, a held one for a
	// revocation. 0 = default 30 s (minimum 1 s).
	LeasePollS time.Duration
	// IdleS is how long the worker may sit without a request before
	// Unload is called to free its resources. 0 = never idle-unload.
	IdleS time.Duration
	// Load brings the worker's resources back after an idle unload,
	// before the next request is handled. nil = nothing to load
	// (resources are always resident).
	Load func(ctx context.Context) error
	// Unload frees the worker's resources after IdleS of inactivity.
	// nil = nothing to unload.
	Unload func(ctx context.Context) error
	// Conn is the broker connection. Required.
	Conn *queue.Conn
	// Handler does the work. Required.
	Handler Handler
	// Events, when set, receives the job.<cap>.* lifecycle events and
	// the worker.<name>.* heartbeat. nil = events are dropped (a worker
	// can run without the events exchange, but then heartbeats are off).
	Events *queue.EventPublisher
	// HeartbeatS is the interval between worker.<name>.heartbeat events;
	// 0 = off. Default 30 s when Events is set.
	HeartbeatS time.Duration
	// MetricsAddr, when set, serves Prometheus /metrics for this worker
	// (deliveries by outcome, handler duration, inflight, loaded, last
	// work). Empty = no metrics endpoint.
	MetricsAddr string
	// OutDirBase is where a request without an explicit OutDir puts its
	// artifacts (and its idempotency marker): <base>/cap-<cap>/<corr>/.
	// Default ~/.local/share/jarvis/artifacts.
	OutDirBase string
	// Log; nil = the standard logger.
	Log *log.Logger
}

// Worker is one capability worker bound to one cap queue.
type Worker struct {
	cfg     Config
	log     *log.Logger
	metrics *Metrics
	// lease is the lease state machine (plan §3.6); nil = un-gated.
	lease *leaseManager

	// loaded and the transition calls (Load/Unload) are guarded by mu:
	// the idle watchdog runs concurrently with the delivery loop, and an
	// unload racing a delivery would free the model mid-request.
	mu     sync.Mutex
	loaded bool
	// inflight is the number of deliveries being handled (0 or 1 —
	// prefetch=1); the watchdog never unloads while one is in flight.
	inflight atomic.Int32
	// lastWork is the unix time the worker last picked up a request.
	lastWork atomic.Int64
}

// New fills in defaults and returns a ready-to-Run worker.
func New(cfg Config) *Worker {
	if cfg.OutDirBase == "" {
		home, _ := os.UserHomeDir()
		cfg.OutDirBase = filepath.Join(home, ".local", "share", "jarvis", "artifacts")
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	if cfg.HeartbeatS <= 0 && cfg.Events != nil {
		cfg.HeartbeatS = 30 * time.Second
	}
	w := &Worker{cfg: cfg, log: cfg.Log, metrics: NewMetrics()}
	if cfg.LeaseBroker != nil && cfg.LeaseSpec.Capability != "" {
		poll := cfg.LeasePollS
		if poll <= 0 {
			poll = 30 * time.Second
		}
		w.lease = newLeaseManager(cfg.LeaseBroker, cfg.LeaseSpec, poll, cfg.Log)
	}
	// A worker starts with its resources resident: the first idle
	// window is measured from startup, not from the first request.
	w.loaded = true
	w.lastWork.Store(time.Now().UnixNano())
	return w
}

// QueueName is the cap queue this worker consumes (cap.<cap>).
func (w *Worker) QueueName() string { return queue.CapQueueName(w.cfg.Cap) }

// LocationPriority maps a worker location to its consumer's AMQP
// priority: GPU 10, iGPU 5, CPU 1 (and off-box "remote:<host>" workers
// 1 — a slow link must not cut in front of a local CPU worker).
// Consumer priority is a pure ranking of consumers: the broker delivers
// each message to the highest-priority consumer that is free to take it
// (internal/queue's consumer_live_test probes the broker: there is no
// gating on the message's own priority — that one only orders the ready
// messages within the queue). So a free GPU worker takes the work and a
// message falls to the cheaper placement while the GPU worker is busy —
// the best free worker, with no scheduler in between (plan §3.5).
func LocationPriority(location string) int {
	switch location {
	case "gpu":
		return 10
	case "igpu":
		return 5
	default:
		// cpu, remote:<host>, and empty (a worker that did not say
		// where it runs is treated as a cheap local one).
		return 1
	}
}

// effectivePriority is the x-priority this worker's consumer declares.
func (w *Worker) effectivePriority() int {
	if w.cfg.Priority > 0 {
		return w.cfg.Priority
	}
	return LocationPriority(w.cfg.Location)
}

// ensureLease moves the lease state machine forward and reports whether
// the worker may consume (plan §3.6): a granted lease, or a fail-open
// assumption while resourced is unreachable. A worker without a broker
// is un-gated: it always may (the pre-lease behavior).
func (w *Worker) ensureLease(ctx context.Context) bool {
	if w.lease == nil {
		return true
	}
	return w.lease.ensureLease(ctx)
}

// releaseLease gives a held lease back (the idle-unload path: the
// worker's resources are free, so the ledger's remainder is real again).
// A no-op without a broker.
func (w *Worker) releaseLease(ctx context.Context) {
	if w.lease == nil {
		return
	}
	w.lease.releaseHeld(ctx)
}

// leaseTick advances the lease state machine on the poll interval: a
// denied or lost lease is re-asked, a queued one checked for its grant,
// a held one for a revocation (the window controller's task-29 path),
// and a fail-open one re-requested so the decision becomes real when
// resourced answers again.
func (w *Worker) leaseTick(ctx context.Context) {
	interval := w.cfg.LeasePollS
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.lease.tick(ctx)
		}
	}
}

// isLoaded reports whether the worker's resources are currently loaded.
func (w *Worker) isLoaded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loaded
}

func (w *Worker) lastWorkTime() time.Time {
	return time.Unix(0, w.lastWork.Load()).Local()
}

// Run declares the queue and consumes it until ctx is cancelled. It
// re-opens the broker connection after a broker restart (the queue is
// durable, so a message that arrived while the worker or the broker was
// down is still on the queue when the worker comes back — invariant 3)
// and gates every consume session on the worker's resource lease (plan
// §3.6): without a lease the worker does not consume, and its messages
// wait for a lower-priority placement of the same capability.
func (w *Worker) Run(ctx context.Context) error {
	if w.cfg.Cap == "" {
		return errors.New("capworker: Cap is required")
	}
	if w.lease != nil {
		defer func() {
			// Release on exit: the ledger is in-memory with no TTL, so an
			// orphaned lease (held or queued) would block this worker's
			// fresh request after a restart. A fresh context — ctx is
			// cancelled here.
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			w.lease.release(rctx)
		}()
		go w.leaseTick(ctx)
	}
	if err := w.cfg.Conn.EnsureCapQueue(w.QueueName()); err != nil {
		return fmt.Errorf("capworker: declare %s: %w", w.QueueName(), err)
	}
	if w.cfg.MetricsAddr != "" {
		go w.metricsServer(ctx)
	}
	if w.cfg.HeartbeatS > 0 && w.cfg.Events != nil {
		go w.heartbeatLoop(ctx)
	}
	if w.cfg.IdleS > 0 && w.cfg.Unload != nil {
		go w.idleWatchdog(ctx)
	}
	w.log.Printf("capworker %s: consuming %s (priority %d, location %q)", w.cfg.Cap, w.QueueName(), w.effectivePriority(), w.cfg.Location)
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if w.cfg.Conn.Dead() {
			if err := w.cfg.Conn.Reconnect(); err != nil {
				w.log.Printf("capworker %s: broker unreachable, retrying: %v", w.cfg.Cap, err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = time.Second
			w.log.Printf("capworker %s: reconnected, resuming %s", w.cfg.Cap, w.QueueName())
		}
		if !w.ensureLease(ctx) {
			// No lease (plan §3.6): do not consume. The messages stay on
			// the durable queue, and a lower-priority consumer of the same
			// capability can still take them. The state machine re-asks on
			// its own poll interval (and logs the denial reason per ask);
			// back off here so a denied worker does not spin.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if err := w.consume(ctx); err != nil && ctx.Err() == nil {
			// The channel or connection died mid-consume (broker restart,
			// network drop, the queue is gone): the unacked delivery is
			// requeued by the broker, and the loop retries on the next
			// pass. Sleep a beat first — retrying a dead channel in a
			// tight loop just spins (the queue may not even exist yet).
			w.log.Printf("capworker %s: consume: %v", w.cfg.Cap, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
		}
	}
}

// consume is one broker session on the cap queue: a dedicated channel,
// prefetch=1 (one in-flight request — a slow capability must not queue up
// behind itself), a consumer priority per location (x-priority — the
// broker delivers each message to the best free worker, plan §3.5), and
// one delivery at a time. The consumer carries a named tag: when the
// worker's lease is lost (revoked) it cancels the consumer — unacked
// deliveries requeue — so the queue falls to a lower-priority placement
// (plan §3.6).
func (w *Worker) consume(ctx context.Context) error {
	ch, err := w.cfg.Conn.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Qos(1, 0, true); err != nil {
		return fmt.Errorf("qos: %w", err)
	}
	tag := fmt.Sprintf("%s-%d", w.cfg.Name, time.Now().UnixNano())
	dels, err := ch.Consume(w.QueueName(), tag, false, false, false, false, amqp.Table{"x-priority": w.effectivePriority()})
	if err != nil {
		return err
	}
	// The lease-loss channel (nil without a broker): closed when the held
	// lease is lost — revoked by the window controller, a fail-open
	// re-sync that comes back denied, or an idle unload's release.
	var lost <-chan struct{}
	if w.lease != nil {
		lost = w.lease.lost()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-lost:
			// The lease is gone: cancel the consumer and let the Run loop
			// re-ask (the in-flight delivery, if any, was handled before
			// the loss — prefetch=1, one delivery at a time).
			w.log.Printf("capworker %s: lease lost, cancelling consumer", w.cfg.Cap)
			if cerr := ch.Cancel(tag, true); cerr != nil {
				w.log.Printf("capworker %s: cancel consumer: %v", w.cfg.Cap, cerr)
			}
			return nil
		case del, ok := <-dels:
			if !ok {
				return errors.New("delivery channel closed")
			}
			w.handle(ctx, del)
			if lost != nil && !w.lease.holding() {
				// The lease was lost while this delivery was handled
				// (select picked the delivery over the closed loss
				// channel): the current request is drained — cancel so
				// the queue falls to the next placement (plan §3.6).
				w.log.Printf("capworker %s: lease lost after delivery, cancelling consumer", w.cfg.Cap)
				if cerr := ch.Cancel(tag, true); cerr != nil {
					w.log.Printf("capworker %s: cancel consumer: %v", w.cfg.Cap, cerr)
				}
				return nil
			}
		}
	}
}

// handle applies the envelope's contract to one delivery.
func (w *Worker) handle(ctx context.Context, del amqp.Delivery) {
	w.inflight.Add(1)
	defer w.inflight.Add(-1)
	w.metrics.SetInflight(float64(w.inflight.Load()))
	w.lastWork.Store(time.Now().UnixNano())
	w.metrics.SetLastWork(time.Now())
	req, err := queue.DecodeMessage(del.Body)
	if err != nil {
		w.log.Printf("capworker %s: undecodable message, dead-lettering: %v", w.cfg.Cap, err)
		w.emit(req, "failed", map[string]any{"error": "undecodable message"})
		del.Nack(false, false) // → DLX (invariant 9)
		return
	}
	if req.CorrelationID == "" {
		// No correlation id means no idempotency key and no way to match
		// a result: dead-letter it with the reason (invariant 9).
		w.log.Printf("capworker %s: message without correlation_id, dead-lettering", w.cfg.Cap)
		w.emit(req, "failed", map[string]any{"error": "no correlation_id"})
		del.Nack(false, false)
		return
	}

	// The lease is gone (revoked, or a fail-open re-sync that came back
	// denied): this delivery may have arrived before the consumer's
	// cancel landed. Requeue it — the work goes to a placement that
	// holds a lease (plan §3.6). The consume loop cancels the consumer
	// right after.
	if w.lease != nil && !w.lease.holding() {
		w.log.Printf("capworker %s: %s: lease not held, requeueing", w.cfg.Cap, req.CorrelationID)
		del.Nack(false, true)
		return
	}

	// Fill in the envelope's defaults before the handler sees the request:
	// the handler writes its artifacts to req.OutDir.
	if req.OutDir == "" {
		req.OutDir = filepath.Join(w.cfg.OutDirBase, "cap-"+w.cfg.Cap, req.CorrelationID)
	}
	outDir := req.OutDir

	// Invariant 4: a redelivery must not redo work. The marker stores the
	// Result, so the reply can be re-sent without re-running the handler.
	marker := queue.DoneMarker(outDir, req.CorrelationID)
	if b, merr := os.ReadFile(marker); merr == nil {
		res, rerr := queue.DecodeResult(b)
		w.log.Printf("capworker %s: %s already done (redelivery); republishing stored result", w.cfg.Cap, req.CorrelationID)
		if rerr == nil {
			if perr := w.publishResult(ctx, req.ReplyTo, res); perr != nil {
				// The work is done (the marker proves it) and the re-reply
				// failed: dead-letter with the reason rather than blocking
				// the queue unacked. A requeue of the dead letter re-runs
				// this same path — re-reply from the marker, no redo.
				w.log.Printf("capworker %s: re-reply %s: %v — dead-lettering", w.cfg.Cap, req.CorrelationID, perr)
				w.emit(req, "failed", map[string]any{"error": "re-reply failed: " + perr.Error()})
				del.Nack(false, false)
				return
			}
		} else {
			// The marker exists but its stored result is unreadable: the
			// work was done (do not redo it — invariant 4) and the reply
			// cannot be rebuilt. Ack and let the flow queue's own
			// deadline (the switchboard's timeout wake) close the loop.
			w.log.Printf("capworker %s: marker %s unreadable (%v); acking without re-reply", w.cfg.Cap, marker, rerr)
		}
		del.Ack(false)
		return
	}

	// Deadline passed before pickup: a terminal timeout. The switchboard
	// already wakes the job at the deadline; this keeps the flow queue's
	// record consistent (invariant 7/9: a result exists for the request).
	// No load needed — the model never runs.
	if req.Deadline != nil && !req.Deadline.After(time.Now()) {
		res := queue.Result{
			Status: queue.ResultTimeout,
			Error:  "deadline passed before the worker picked up the request",
		}
		w.metrics.Observe(queue.ResultTimeout, time.Since(w.lastWorkTime()))
		w.finish(ctx, del, req, outDir, res)
		return
	}

	// An idle-unloaded worker brings its resources back before handling
	// (Load runs under w.mu, so it cannot race the watchdog's Unload).
	// A load failure is handled like a handler failure: it burns an
	// attempt, and the retry may find the resource available.
	if lerr := w.ensureLoaded(ctx); lerr != nil {
		w.retryOrFail(ctx, del, req, lerr)
		return
	}

	started := time.Now()
	res, herr := w.cfg.Handler.Handle(ctx, req)
	w.metrics.Observe(outcomeOf(herr, res), time.Since(started))
	if herr != nil {
		w.retryOrFail(ctx, del, req, herr)
		return
	}
	w.log.Printf("capworker %s: %s done in %s (status %s)", w.cfg.Cap, req.CorrelationID, time.Since(started).Round(time.Millisecond), res.Status)
	res.StartedAt = started
	w.finish(ctx, del, req, outDir, res)
}

// outcomeOf maps a handler outcome to the metric's outcome label.
func outcomeOf(herr error, res queue.Result) string {
	if herr != nil {
		return "error"
	}
	if res.Status == "" {
		res.Status = queue.ResultOK
	}
	return res.Status
}

// finish is the terminal success path (status ok, or a deadline timeout):
// stamp the envelope fields, write the idempotency marker, publish the
// Result in confirm mode, emit the done event, and ack. The marker is
// written BEFORE the reply: a crash in between leaves a redelivery that
// re-replies from the marker instead of redoing the work (invariant 3).
// If the publish itself fails the delivery is dead-lettered (the marker
// keeps any future redelivery from redoing the work).
func (w *Worker) finish(ctx context.Context, del amqp.Delivery, req queue.Message, outDir string, res queue.Result) {
	res = w.stamp(req, res)
	if merr := writeMarker(outDir, req.CorrelationID, res); merr != nil {
		w.log.Printf("capworker %s: %s: write marker: %v", w.cfg.Cap, req.CorrelationID, merr)
	}
	if req.ReplyTo == "" {
		w.log.Printf("capworker %s: %s has no ReplyTo; result kept at %s, acking", w.cfg.Cap, req.CorrelationID, outDir)
		w.emit(req, "failed", map[string]any{"error": "no reply-to"})
		del.Ack(false)
		return
	}
	if perr := w.publishResult(ctx, req.ReplyTo, res); perr != nil {
		w.log.Printf("capworker %s: reply %s: %v — dead-lettering", w.cfg.Cap, req.CorrelationID, perr)
		w.emit(req, "failed", map[string]any{"error": "reply failed: " + perr.Error()})
		del.Nack(false, false)
		return
	}
	w.emit(req, "done", map[string]any{"status": res.Status, "worker": res.Worker, "artifacts": res.Artifacts})
	if aerr := del.Ack(false); aerr != nil {
		// The reply is on the flow queue (confirmed); only the ack was
		// lost. The redelivery republishes the stored result — harmless,
		// the marker makes it a no-op re-reply.
		w.log.Printf("capworker %s: ack %s: %v (redelivery will re-reply from the marker)", w.cfg.Cap, req.CorrelationID, aerr)
	}
}

// stamp fills the envelope fields a handler is not expected to set.
func (w *Worker) stamp(req queue.Message, res queue.Result) queue.Result {
	if res.CorrelationID == "" {
		res.CorrelationID = req.CorrelationID
	}
	if res.Capability == "" {
		res.Capability = req.Capability
	}
	if res.Worker == "" {
		res.Worker = w.cfg.Name
	}
	if res.Model == "" {
		res.Model = w.cfg.Model
	}
	if res.StartedAt.IsZero() {
		res.StartedAt = time.Now().UTC()
	}
	if res.FinishedAt.IsZero() {
		res.FinishedAt = time.Now().UTC()
	}
	return res
}

// retryOrFail burns an attempt on a handler error (the shared delivery
// budget, queue/dlx.go). Under the budget: republish with the bumped
// x-attempts header — the copy is confirmed by the broker before the
// failed delivery is acked, so the message cannot be lost. At the budget:
// nack without requeue, which dead-letters the message (reason "rejected").
func (w *Worker) retryOrFail(ctx context.Context, del amqp.Delivery, req queue.Message, herr error) {
	n := queue.AttemptCount(del) + 1
	if n >= queue.MaxAttempts {
		w.log.Printf("capworker %s: %s failed %d/%d times, dead-lettering: %v", w.cfg.Cap, req.CorrelationID, n, queue.MaxAttempts, herr)
		w.emit(req, "failed", map[string]any{"error": herr.Error(), "attempts": n})
		del.Nack(false, false)
		return
	}
	ch, err := w.cfg.Conn.NewChannel()
	if err != nil {
		w.log.Printf("capworker %s: %s: retry channel: %v (leaving unacked)", w.cfg.Cap, req.CorrelationID, err)
		return
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		w.log.Printf("capworker %s: %s: confirm mode: %v (leaving unacked)", w.cfg.Cap, req.CorrelationID, err)
		return
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub := amqp.Publishing{
		Body:          del.Body,
		ContentType:   del.ContentType,
		CorrelationId: del.CorrelationId,
		ReplyTo:       del.ReplyTo,
		Priority:      del.Priority,
		DeliveryMode:  amqp.Persistent,
		Timestamp:     time.Now(),
		Headers:       queue.BumpAttempt(del),
	}
	// The cap queue is bound to the tasks exchange on <name>.#; the
	// .default routing key is the one jarvis-mq submit publishes with.
	routingKey := w.QueueName() + ".default"
	if err := ch.PublishWithContext(ctx, w.cfg.Conn.Exchange(), routingKey, false, false, pub); err != nil {
		w.log.Printf("capworker %s: %s: retry republish: %v — requeueing", w.cfg.Cap, req.CorrelationID, err)
		del.Nack(false, true)
		return
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			w.log.Printf("capworker %s: %s: broker nacked the retry — requeueing", w.cfg.Cap, req.CorrelationID)
			del.Nack(false, true)
			return
		}
	case <-time.After(resultConfirmTimeout):
		w.log.Printf("capworker %s: %s: retry confirm timed out — requeueing", w.cfg.Cap, req.CorrelationID)
		del.Nack(false, true)
		return
	case <-ctx.Done():
		w.log.Printf("capworker %s: %s: retry cancelled (leaving unacked)", w.cfg.Cap, req.CorrelationID)
		return
	}
	if err := del.Ack(false); err != nil {
		w.log.Printf("capworker %s: %s: ack after retry: %v (the retry is already on the queue)", w.cfg.Cap, req.CorrelationID, err)
	}
	w.log.Printf("capworker %s: %s failed (attempt %d/%d), republished: %v", w.cfg.Cap, req.CorrelationID, n, queue.MaxAttempts, herr)
}

// publishResult publishes the Result to the target queue (the request's
// ReplyTo, a flow queue) in confirm mode and blocks until the broker
// confirms it. A missing flow queue is re-declared first — it is durable,
// and a result published before anyone listens is held on the broker.
func (w *Worker) publishResult(ctx context.Context, target string, res queue.Result) error {
	if target == "" {
		return errors.New("request has no ReplyTo")
	}
	if queue.IsFlowQueue(target) {
		if err := w.cfg.Conn.EnsureFlowQueue(target); err != nil {
			return fmt.Errorf("ensure %s: %w", target, err)
		}
	}
	ch, err := w.cfg.Conn.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("confirm mode: %w", err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	body, err := queue.EncodeResult(res)
	if err != nil {
		return err
	}
	pub := amqp.Publishing{
		Body:          body,
		ContentType:   "application/json",
		CorrelationId: res.CorrelationID,
		DeliveryMode:  amqp.Persistent,
		Timestamp:     time.Now(),
	}
	if err := ch.PublishWithContext(ctx, "", target, false, false, pub); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return errors.New("broker nacked the result")
		}
	case <-time.After(resultConfirmTimeout):
		return errors.New("timed out waiting for the result confirm")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// emit fires a lifecycle event (job.<cap>.<state>) off the request.
// Best-effort: an event outage must not wedge the delivery loop.
func (w *Worker) emit(req queue.Message, state string, data map[string]any) {
	if w.cfg.Events == nil {
		return
	}
	if err := w.cfg.Events.Publish(context.Background(), queue.JobEvent(req, state, data)); err != nil {
		w.log.Printf("capworker %s: event job.%s.%s: %v", w.cfg.Cap, req.Capability, state, err)
	}
}

// writeMarker stores the Result at the idempotency marker path
// (<out_dir>/.done-<corr>, queue.DoneMarker). Redeliveries read it back.
func writeMarker(outDir, corr string, res queue.Result) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	b, err := queue.EncodeResult(res)
	if err != nil {
		return err
	}
	return os.WriteFile(queue.DoneMarker(outDir, corr), b, 0o644)
}

// ensureLoaded brings the worker's resources back after an idle unload,
// before a request that needs them. It runs Load under w.mu so it cannot
// race the watchdog's Unload; a worker with nothing to load returns at
// once.
func (w *Worker) ensureLoaded(ctx context.Context) error {
	if w.cfg.Load == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaded {
		return nil
	}
	w.log.Printf("capworker %s: loading resources (idle-unloaded)", w.cfg.Cap)
	if err := w.cfg.Load(ctx); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	w.loaded = true
	w.metrics.SetLoaded(true)
	w.workerEvent("loaded", map[string]any{"capability": w.cfg.Cap, "location": w.cfg.Location})
	return nil
}

// idleWatchdog ticks and unloads the worker's resources after IdleS
// without a request (the model weights, a GPU process — see
// docs/brain-swap.md for why a worker must give the 4090 back).
func (w *Worker) idleWatchdog(ctx context.Context) {
	interval := w.cfg.IdleS / 2
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.maybeUnload(ctx)
		}
	}
}

// maybeUnload frees the worker's resources after IdleS of inactivity,
// unless a delivery is in flight (the model must not be unloaded
// mid-request). An unload failure leaves the worker loaded — the next
// tick retries.
func (w *Worker) maybeUnload(ctx context.Context) {
	if w.cfg.Unload == nil || w.inflight.Load() != 0 || !w.isLoaded() {
		return
	}
	idle := time.Since(w.lastWorkTime())
	if idle < w.cfg.IdleS {
		return
	}
	w.mu.Lock()
	if !w.loaded {
		w.mu.Unlock()
		return
	}
	w.log.Printf("capworker %s: idle for %s, unloading", w.cfg.Cap, idle.Round(time.Second))
	if err := w.cfg.Unload(ctx); err != nil {
		w.log.Printf("capworker %s: unload failed, staying loaded: %v", w.cfg.Cap, err)
		w.mu.Unlock()
		return
	}
	w.loaded = false
	w.metrics.SetLoaded(false)
	w.workerEvent("unloaded", map[string]any{"capability": w.cfg.Cap, "location": w.cfg.Location, "idle_s": int(idle / time.Second)})
	w.mu.Unlock()
	// The resources are free: give the lease back so the ledger's
	// remainder is real again (plan §3.6). Outside the critical section —
	// the release is an HTTP round-trip to resourced. The worker re-asks
	// on the Run loop's next pass (deferred by a poll interval) before it
	// consumes again.
	w.releaseLease(ctx)
}

// heartbeatLoop emits one worker.<name>.heartbeat event every
// HeartbeatS: the cheap signal that the worker process is alive, where
// it sits (loaded/idle/inflight), and how long since its last request.
func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.HeartbeatS)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.workerEvent("heartbeat", map[string]any{
				"capability": w.cfg.Cap,
				"location":   w.cfg.Location,
				"priority":   w.effectivePriority(),
				"loaded":     w.isLoaded(),
				"inflight":   w.inflight.Load(),
				"last_work":  w.lastWorkTime().UTC().Format(time.RFC3339),
			})
		}
	}
}

// workerEvent emits a worker.<name>.<state> event (heartbeat/loaded/
// unloaded) off the worker's identity. Best-effort, like emit: an events
// outage must not wedge the delivery loop.
func (w *Worker) workerEvent(state string, data map[string]any) {
	if w.cfg.Events == nil {
		return
	}
	if err := w.cfg.Events.Publish(context.Background(), queue.WorkerEvent(w.cfg.Name, state, data)); err != nil {
		w.log.Printf("capworker %s: event worker.%s.%s: %v", w.cfg.Cap, w.cfg.Name, state, err)
	}
}

// metricsServer serves this worker's Prometheus /metrics on
// cfg.MetricsAddr until ctx is done. A bind failure is logged, not
// fatal — metrics are an observation point, not a dependency.
func (w *Worker) metricsServer(ctx context.Context) {
	srv := &http.Server{Addr: w.cfg.MetricsAddr, Handler: w.metrics.Handler()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		w.log.Printf("capworker %s: metrics %s: %v", w.cfg.Cap, w.cfg.MetricsAddr, err)
	}
}
