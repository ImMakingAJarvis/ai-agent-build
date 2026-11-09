package switchboard

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// Defaults. Polling is tight enough to meet the "wakes within 5 s" acceptance
// bar with a wide margin (a result is seen within ~250 ms of landing); the
// deadline check is slower because a wait is measured in minutes, not
// milliseconds.
const (
	DefaultPollInterval    = 250 * time.Millisecond
	DefaultTimeoutInterval = 5 * time.Second
	DefaultMaxBatch        = 100
	// forwardConfirmTimeout bounds how long a forward waits for the broker to
	// confirm the republish before giving up (and requeueing the source).
	forwardConfirmTimeout = 10 * time.Second
)

// Config wires the switchboard's dependencies.
type Config struct {
	Conn   *queue.Conn
	Jobs   *agentjob.Store
	Listen *Store
	M      *Metrics

	// Notify sends a Discord notification for a notify listener (and for a
	// timeout whose TimeoutAction is notify). nil = notify is logged, not sent
	// (the message is acked so it is not retried forever).
	Notify func(ctx context.Context, text string) error

	// PollInterval is how often the listener queues are peeked.
	PollInterval time.Duration
	// TimeoutInterval is how often waiting-job deadlines are checked.
	TimeoutInterval time.Duration
	// MaxBatch is the max messages fetched from one queue per tick.
	MaxBatch int

	// Log; nil = the standard logger.
	Log *log.Logger
}

// Switchboard peeks every queue that has a listener and performs the listener
// action when a message lands. It runs in a single goroutine so the file-backed
// listener and job stores are never touched concurrently.
type Switchboard struct {
	cfg Config
	log *log.Logger
}

// New fills in defaults and returns a ready-to-Run switchboard.
func New(cfg Config) *Switchboard {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.TimeoutInterval <= 0 {
		cfg.TimeoutInterval = DefaultTimeoutInterval
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = DefaultMaxBatch
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &Switchboard{cfg: cfg, log: cfg.Log}
}

// Run polls the listener queues and checks waiting-job deadlines until ctx is
// cancelled. It does an immediate first pass so a result that already landed
// wakes its job promptly on startup (a restart must not delay a ready wake).
func (sb *Switchboard) Run(ctx context.Context) error {
	sb.log.Printf("switchboard: started (poll %s, deadline check %s, batch %d)",
		sb.cfg.PollInterval, sb.cfg.TimeoutInterval, sb.cfg.MaxBatch)
	poll := time.NewTicker(sb.cfg.PollInterval)
	defer poll.Stop()
	timeout := time.NewTicker(sb.cfg.TimeoutInterval)
	defer timeout.Stop()

	sb.pollQueues(ctx)
	sb.checkTimeouts(ctx)
	for {
		select {
		case <-ctx.Done():
			sb.log.Printf("switchboard: stopping")
			return ctx.Err()
		case <-poll.C:
			sb.pollQueues(ctx)
		case <-timeout.C:
			sb.checkTimeouts(ctx)
		}
	}
}

// pollQueues resubscribes on a broker restart, then peeks every queue that has
// a listener and dispatches the messages it finds.
func (sb *Switchboard) pollQueues(ctx context.Context) {
	// Resubscribe: the plan calls for re-attaching every listener after a
	// broker restart. We use Get (peek) rather than amqp.Consume for the
	// actual reads — see handleQueue — so "resubscribe" means re-opening the
	// connection and resuming polling. The listener set is re-read from disk
	// on every pass, so listeners created while the broker was down are
	// picked up too (invariant 3).
	if sb.cfg.Conn.Dead() {
		if err := sb.cfg.Conn.Reconnect(); err != nil {
			sb.cfg.M.ListenersActive.Set(0)
			sb.log.Printf("switchboard: broker unreachable, retrying next pass: %v", err)
			return
		}
		sb.cfg.M.Resubscribes.Inc()
		sb.log.Printf("switchboard: reconnected to broker, resuming listener queues")
	}
	byQueue, err := sb.cfg.Listen.Queues()
	if err != nil {
		sb.log.Printf("switchboard: read listeners: %v", err)
		return
	}
	n := 0
	for _, ls := range byQueue {
		n += len(ls)
	}
	sb.cfg.M.ListenersActive.Set(float64(n))
	for qname, listeners := range byQueue {
		sb.handleQueue(ctx, qname, listeners)
	}
}

// arrival is a decoded delivery on a watched queue: its correlation id and a
// short reference to the result (artifact paths, summary, or message id).
type arrival struct {
	corr string
	ref  string
}

// decodeArrival turns a delivery body into an arrival. Results on flow queues
// are queue.Result; a DecodeMessage of a Result body does not error, so the
// discriminator is Status != "".
func decodeArrival(body []byte) (arrival, error) {
	res, err := queue.DecodeResult(body)
	if err == nil && res.Status != "" {
		ref := strings.Join(res.Artifacts, " ")
		if ref == "" {
			ref = res.Summary
		}
		if ref == "" {
			ref = "status=" + res.Status
		}
		return arrival{corr: res.CorrelationID, ref: ref}, nil
	}
	m, err := queue.DecodeMessage(body)
	if err != nil {
		return arrival{}, err
	}
	ref := m.ID
	if ref == "" {
		ref = "msg"
	}
	return arrival{corr: m.CorrelationID, ref: ref}, nil
}

// handleQueue peeks one listened queue (a batch of Get, autoAck=false) and
// applies each message's fate.
//
// Why Get (polling) and not amqp.Consume: a wake listener must PEEK — it reads
// the result and wakes the job but leaves the message for the woken round to
// consume and ack. A consumer with prefetch=1 that peeks the front message and
// requeues it would redeliver the very message it just read on the next tick —
// a redelivery loop. Get (the exact primitive PeekFlow uses) has no prefetch
// slot to redeliver into: we fetch a batch, decide each fate, then ack the
// removed and requeue the kept in reverse (restoring front-of-queue order). If
// the process dies mid-pass the broker requeues the unacked batch, so nothing
// is lost.
func (sb *Switchboard) handleQueue(ctx context.Context, qname string, listeners []Listener) {
	ch, err := sb.cfg.Conn.NewChannel()
	if err != nil {
		sb.log.Printf("switchboard: %s: channel: %v", qname, err)
		return
	}
	defer ch.Close()

	var dels []amqp.Delivery
	for len(dels) < sb.cfg.MaxBatch {
		del, ok, err := ch.Get(qname, false)
		if err != nil {
			if isNotFound(err) {
				return // the queue was deleted this pass; nothing to do
			}
			sb.log.Printf("switchboard: %s: get: %v", qname, err)
			return
		}
		if !ok {
			break // queue empty
		}
		dels = append(dels, del)
	}
	if len(dels) == 0 {
		return
	}

	// Decide each fate, then apply. A message is either acked (removed),
	// requeued (kept, retried next pass), or dead-lettered (undecodable).
	keep := make([]bool, len(dels))
	dead := make([]bool, len(dels))
	for i := range dels {
		arr, err := decodeArrival(dels[i].Body)
		if err != nil {
			sb.log.Printf("switchboard: %s: undecodable message, dead-lettering: %v", qname, err)
			dead[i] = true
			continue
		}
		if !anyMatch(listeners, arr.corr) {
			// A decodable message no listener claims: leave it. We never
			// destroy a message we did not act on.
			sb.log.Printf("switchboard: %s: no listener matches corr %q; leaving message", qname, arr.corr)
			keep[i] = true
			continue
		}
		if sb.dispatch(ctx, qname, dels[i], listeners, arr) {
			keep[i] = true
		}
	}

	// Apply: dead-letter the undecodable, ack the removed, then requeue the
	// kept in reverse so their relative front-of-queue order is preserved.
	for i := range dels {
		if dead[i] {
			dels[i].Nack(false, false)
		} else if !keep[i] {
			dels[i].Ack(false)
		}
	}
	for i := len(dels) - 1; i >= 0; i-- {
		if keep[i] {
			dels[i].Nack(false, true)
		}
	}
}

// anyMatch reports whether at least one listener on the queue claims this
// correlation id (an empty CorrFilter claims any message).
func anyMatch(listeners []Listener, corr string) bool {
	for _, l := range listeners {
		if l.CorrFilter == "" || l.CorrFilter == corr {
			return true
		}
	}
	return false
}

// dispatch runs every listener that matches the delivery and reports whether
// the message must be kept (a wake needs it to stay, or an action failed and
// should be retried).
func (sb *Switchboard) dispatch(ctx context.Context, qname string, del amqp.Delivery, listeners []Listener, arr arrival) bool {
	keep := false
	for _, l := range listeners {
		if l.CorrFilter != "" && l.CorrFilter != arr.corr {
			continue
		}
		switch l.Action {
		case ActionWake:
			sb.doWake(l, arr.corr, arr.ref)
			keep = true // a wake always leaves the message for the woken round
		case ActionForward:
			if err := sb.forward(ctx, del, l.TargetQueue); err != nil {
				sb.log.Printf("switchboard: forward %s -> %s failed, requeue: %v", qname, l.TargetQueue, err)
				keep = true
			} else {
				sb.cfg.M.ForwardsTotal.Inc()
				sb.log.Printf("switchboard: forwarded a message %s -> %s", qname, l.TargetQueue)
			}
		case ActionNotify:
			text := l.Text
			if text == "" {
				text = fmt.Sprintf("switchboard: message on %s", qname)
			}
			if err := sb.notify(ctx, text); err != nil {
				sb.log.Printf("switchboard: notify on %s failed, requeue: %v", qname, err)
				keep = true
			} else {
				sb.cfg.M.NotifiesTotal.Inc()
				sb.log.Printf("switchboard: notified on %s", qname)
			}
		}
	}
	return keep
}

// doWake records the result against the job and wakes it. It is a no-op when
// the job is not waiting (already woken, or never was) — that short-circuit is
// what keeps post-wake polls cheap.
func (sb *Switchboard) doWake(l Listener, corr, ref string) {
	j, err := sb.cfg.Jobs.Get(l.JobID)
	if err != nil {
		sb.log.Printf("switchboard: wake %s: job %s: %v", l.ID, l.JobID, err)
		return
	}
	if j.Status != agentjob.Waiting {
		return // not waiting: the result is already recorded / the round owns the message
	}
	if corr != "" {
		if err := sb.cfg.Jobs.RecordResult(l.JobID, corr, ref); err != nil {
			sb.log.Printf("switchboard: wake %s: record %s for %s: %v", l.ID, corr, l.JobID, err)
			return
		}
	}
	rec, err := sb.cfg.Jobs.Wake(l.JobID, ref)
	if err != nil {
		if errors.Is(err, agentjob.ErrWaitOnUnmet) {
			return // still waiting on other corr(s); expected
		}
		sb.log.Printf("switchboard: wake %s: wake %s: %v", l.ID, l.JobID, err)
		return
	}
	sb.cfg.M.WakesTotal.Inc()
	sb.log.Printf("switchboard: woke job %s (%s) via listener %s", l.JobID, rec.Reason, l.ID)
}

// forward republishes the raw delivery to target (direct, empty exchange) in
// confirm mode and blocks until the broker confirms the copy — the source is
// acked only after this returns nil.
func (sb *Switchboard) forward(ctx context.Context, del amqp.Delivery, target string) error {
	if err := sb.ensureTarget(target); err != nil {
		return err
	}
	ch, err := sb.cfg.Conn.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("confirm mode: %w", err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	pub := amqp.Publishing{
		Body:          del.Body,
		ContentType:   del.ContentType,
		CorrelationId: del.CorrelationId,
		ReplyTo:       del.ReplyTo,
		DeliveryMode:  del.DeliveryMode,
		Priority:      del.Priority,
		Timestamp:     del.Timestamp,
		Headers:       del.Headers,
	}
	if pub.DeliveryMode == 0 {
		pub.DeliveryMode = amqp.Persistent
	}
	if err := ch.PublishWithContext(ctx, "", target, false, false, pub); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return errors.New("broker nacked the forward")
		}
	case <-time.After(forwardConfirmTimeout):
		return errors.New("timed out waiting for forward confirm")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ensureTarget makes sure the forward target exists. Flow and capability
// queues are declared with their dead-letter args; anything else is declared
// as a plain durable queue. The Ensure*Queue helpers declare the name as
// given, and the rest of the repo passes the FULL queue name (flow.x /
// cap.x) to them — so target is passed through, prefix and all.
func (sb *Switchboard) ensureTarget(target string) error {
	switch {
	case queue.IsFlowQueue(target):
		return sb.cfg.Conn.EnsureFlowQueue(target)
	case queue.IsCapQueue(target):
		return sb.cfg.Conn.EnsureCapQueue(target)
	default:
		ch, err := sb.cfg.Conn.NewChannel()
		if err != nil {
			return err
		}
		defer ch.Close()
		_, err = ch.QueueDeclare(target, true, false, false, false, nil)
		return err
	}
}

// notify sends a Discord notification. A nil notifier is treated as success
// (logged elsewhere) so a notify listener is not retried forever when Discord
// is not configured.
func (sb *Switchboard) notify(ctx context.Context, text string) error {
	if sb.cfg.Notify == nil {
		return nil
	}
	return sb.cfg.Notify(ctx, text)
}

// checkTimeouts wakes any waiting job whose deadline has passed. The deadline
// is the job's WaitUntil (set by Yield, invariant 2); the listener's Timeout is
// only a fallback when the job carries no deadline.
func (sb *Switchboard) checkTimeouts(ctx context.Context) {
	listeners, err := sb.cfg.Listen.List()
	if err != nil {
		sb.log.Printf("switchboard: read listeners: %v", err)
		return
	}
	now := time.Now().UTC()
	for _, l := range listeners {
		if l.Action != ActionWake || l.JobID == "" {
			continue
		}
		j, err := sb.cfg.Jobs.Get(l.JobID)
		if err != nil {
			continue // job gone (finished or failed): nothing to time out
		}
		if j.Status != agentjob.Waiting {
			continue
		}
		dl := j.WaitDeadline()
		if dl.IsZero() && l.Timeout > 0 {
			dl = l.CreatedAt.Add(l.Timeout)
		}
		if dl.IsZero() || now.Before(dl) {
			continue
		}
		rec, err := sb.cfg.Jobs.Wake(l.JobID, "")
		if err != nil {
			if errors.Is(err, agentjob.ErrWaitOnUnmet) {
				continue
			}
			sb.log.Printf("switchboard: timeout wake %s: %v", l.JobID, err)
			continue
		}
		if rec.Reason != agentjob.WakeTimeout {
			// A result landed between the deadline check and the wake: it woke
			// on "all", not "timeout". Count it as a normal wake, not a timeout.
			sb.cfg.M.WakesTotal.Inc()
			continue
		}
		sb.cfg.M.TimeoutsTotal.Inc()
		sb.log.Printf("switchboard: job %s timed out waiting (deadline %s)", l.JobID, dl.Format(time.RFC3339))
		if l.TimeoutAction == TimeoutNotify {
			text := l.Text
			if text == "" {
				text = fmt.Sprintf("switchboard: job %s timed out waiting on %s", l.JobID, l.Queue)
			}
			if err := sb.notify(ctx, text); err != nil {
				sb.log.Printf("switchboard: timeout notify for %s failed: %v", l.JobID, err)
			}
		}
	}
}

// isNotFound reports whether err is an AMQP channel error with code 404
// (the queue was deleted). Get on a missing queue must not be treated as a
// real failure — it is the normal state of a queue whose flow has ended.
func isNotFound(err error) bool {
	var pe *amqp.Error
	if errors.As(err, &pe) {
		return pe.Code == int(amqp.NotFound)
	}
	return false
}
