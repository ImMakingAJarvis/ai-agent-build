// Package capworker is the minimal capability-worker SDK (plan task 17;
// the full SDK lands in task 19 — this is its seed, kept small but
// structured so real workers slot into the same shape). A worker names a
// capability ("echo" → cap.echo), implements a Handler, and runs: the SDK
// declares the queue (durable, x-max-priority 10, dead-letter args —
// idempotently, on every start), consumes with prefetch=1, and owns the
// envelope's contract for every delivery:
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
// The SDK also emits the job.<capability>.done / .failed lifecycle events
// (internal/queue events exchange) so other services can watch capability
// work without reading the flow queues.
package capworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	// Conn is the broker connection. Required.
	Conn *queue.Conn
	// Handler does the work. Required.
	Handler Handler
	// Events, when set, receives the job.<cap>.done / job.<cap>.failed
	// events. nil = events are logged, not published.
	Events *queue.EventPublisher
	// OutDirBase is where a request without an explicit OutDir puts its
	// artifacts (and its idempotency marker): <base>/cap-<cap>/<corr>/.
	// Default ~/.local/share/jarvis/artifacts.
	OutDirBase string
	// Log; nil = the standard logger.
	Log *log.Logger
}

// Worker is one capability worker bound to one cap queue.
type Worker struct {
	cfg Config
	log *log.Logger
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
	return &Worker{cfg: cfg, log: cfg.Log}
}

// QueueName is the cap queue this worker consumes (cap.<cap>).
func (w *Worker) QueueName() string { return queue.CapQueueName(w.cfg.Cap) }

// Run declares the queue and consumes it until ctx is cancelled. It
// re-opens the broker connection after a broker restart (the queue is
// durable, so a message that arrived while the worker or the broker was
// down is still on the queue when the worker comes back — invariant 3).
func (w *Worker) Run(ctx context.Context) error {
	if w.cfg.Cap == "" {
		return errors.New("capworker: Cap is required")
	}
	if err := w.cfg.Conn.EnsureCapQueue(w.QueueName()); err != nil {
		return fmt.Errorf("capworker: declare %s: %w", w.QueueName(), err)
	}
	w.log.Printf("capworker %s: consuming %s", w.cfg.Cap, w.QueueName())
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
		if err := w.consume(ctx); err != nil && ctx.Err() == nil {
			// The channel or connection died mid-consume (broker restart,
			// network drop): the unacked delivery is requeued by the
			// broker, and the loop reconnects on the next pass.
			w.log.Printf("capworker %s: consume: %v", w.cfg.Cap, err)
		}
	}
}

// consume is one broker session on the cap queue: a dedicated channel,
// prefetch=1 (one in-flight request — a slow capability must not queue up
// behind itself), and one delivery at a time.
func (w *Worker) consume(ctx context.Context) error {
	ch, err := w.cfg.Conn.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Qos(1, 0, true); err != nil {
		return fmt.Errorf("qos: %w", err)
	}
	dels, err := ch.Consume(w.QueueName(), "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case del, ok := <-dels:
			if !ok {
				return errors.New("delivery channel closed")
			}
			w.handle(ctx, del)
		}
	}
}

// handle applies the envelope's contract to one delivery.
func (w *Worker) handle(ctx context.Context, del amqp.Delivery) {
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
	if req.Deadline != nil && !req.Deadline.After(time.Now()) {
		res := queue.Result{
			Status: queue.ResultTimeout,
			Error:  "deadline passed before the worker picked up the request",
		}
		w.finish(ctx, del, req, outDir, res)
		return
	}

	started := time.Now()
	res, herr := w.cfg.Handler.Handle(ctx, req)
	if herr != nil {
		w.retryOrFail(ctx, del, req, herr)
		return
	}
	res.StartedAt = started
	w.finish(ctx, del, req, outDir, res)
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
