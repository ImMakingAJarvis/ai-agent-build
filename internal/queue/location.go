// Location queues and workers (Phase A6 of docs/work-board/PLAN.md,
// "Per-location queues and workers").
//
// Each box location has its own queue — jobs.gpu, jobs.cpu, jobs.igpu —
// and its own LocationWorker: a dedicated AMQP channel, its own
// prefetch=1, its own consume goroutine and its own pause flag. Jobs on
// different locations therefore run in parallel (a CPU or iGPU job never
// waits behind the GPU job), and pausing one location — the game watchdog
// pauses only the GPU queue — leaves the others running.
package queue

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Location queue names. They live on the same "tasks" topic exchange as
// the priority queues and are bound the same way: routing key q + ".#".
const (
	JobsGPUQueue  = "jobs.gpu"
	JobsCPUQueue  = "jobs.cpu"
	JobsIGPUQueue = "jobs.igpu"
)

// LocationQueues lists the location queues in stable order.
var LocationQueues = []string{JobsGPUQueue, JobsCPUQueue, JobsIGPUQueue}

// LocationQueueFor maps a placement location ("gpu", "cpu", "igpu") to
// its queue name. The second result is false for anything that is not a
// box location — "cloud" runs on the provider's hardware and has no box
// queue, for instance.
func LocationQueueFor(loc string) (string, bool) {
	switch loc {
	case "gpu":
		return JobsGPUQueue, true
	case "cpu":
		return JobsCPUQueue, true
	case "igpu":
		return JobsIGPUQueue, true
	default:
		return "", false
	}
}

// LocationWorker consumes one location queue. It is independent of the
// main Consumer and of the other location workers: its own channel means
// its prefetch does not share the main consumer's single delivery slot,
// so a long job on one location queue cannot hold up another.
type LocationWorker struct {
	conn    *Conn
	queue   string
	handler Handler

	running atomic.Bool
	paused  atomic.Bool
	ch      *amqp.Channel
}

// NewLocationWorker builds a worker for one queue (a name from
// LocationQueues, or a test queue).
func NewLocationWorker(conn *Conn, queue string, handler Handler) *LocationWorker {
	return &LocationWorker{conn: conn, queue: queue, handler: handler}
}

// Start opens a dedicated channel, declares and binds the queue, sets
// prefetch=1 on that channel, and begins consuming.
func (w *LocationWorker) Start() error {
	ch, err := w.conn.NewChannel()
	if err != nil {
		return fmt.Errorf("channel for %s: %w", w.queue, err)
	}
	if _, err := ch.QueueDeclare(w.queue, true, false, false, false, nil); err != nil {
		ch.Close()
		return fmt.Errorf("declare %s: %w", w.queue, err)
	}
	if err := ch.QueueBind(w.queue, w.queue+".#", w.conn.Exchange(), true, nil); err != nil {
		ch.Close()
		return fmt.Errorf("bind %s to %s: %w", w.queue, w.queue+".#", err)
	}
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		return fmt.Errorf("qos on %s: %w", w.queue, err)
	}
	dels, err := ch.Consume(w.queue, "", false, false, false, false, nil)
	if err != nil {
		ch.Close()
		return fmt.Errorf("consume %s: %w", w.queue, err)
	}
	w.ch = ch
	w.running.Store(true)
	go w.consumeLoop(dels)
	log.Printf("location: listening on %s", w.queue)
	return nil
}

// Pause stops the worker from starting new jobs. A job already in flight
// runs to completion; a delivery that arrives while paused is held
// unacked, and with prefetch=1 on this worker's own channel that holding
// is what stops the queue from delivering more.
func (w *LocationWorker) Pause() {
	if w.paused.CompareAndSwap(false, true) {
		log.Printf("location: %s paused", w.queue)
	}
}

// Resume lets a paused worker start jobs again (any held delivery is
// processed next).
func (w *LocationWorker) Resume() {
	if w.paused.CompareAndSwap(true, false) {
		log.Printf("location: %s resumed", w.queue)
	}
}

// IsPaused reports whether the worker is paused.
func (w *LocationWorker) IsPaused() bool { return w.paused.Load() }

// Stop ends the consume loop and closes the worker's channel; a held
// unacked delivery is requeued by the broker on channel close.
func (w *LocationWorker) Stop() {
	w.running.Store(false)
	if w.ch != nil {
		w.ch.Close()
		w.ch = nil
	}
}

func (w *LocationWorker) consumeLoop(dels <-chan amqp.Delivery) {
	for {
		if !w.running.Load() {
			return
		}
		del, ok := <-dels
		if !ok {
			return
		}
		// While paused the delivery stays unacked (see Pause).
		for w.paused.Load() {
			if !w.running.Load() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := w.handleOne(del); err != nil {
			log.Printf("location: %s: %v", w.queue, err)
		}
	}
}

// handleOne mirrors Consumer.handleOne: decode, run the handler, ack on
// success; ErrQueuePaused / ErrApprovalDenied leave the message unacked;
// any other handler error nacks without requeue.
func (w *LocationWorker) handleOne(del amqp.Delivery) error {
	msg, err := DecodeMessage(del.Body)
	if err != nil {
		del.Nack(false, false)
		return fmt.Errorf("decode from %s: %w", w.queue, err)
	}
	log.Printf("location: %s → job %s (type=%s, source=%s)", w.queue, msg.ID, msg.Type, msg.Source)
	if err := w.handler(msg); err != nil {
		if err == ErrQueuePaused || err == ErrApprovalDenied {
			return nil // stays unacked, same as the main consumer
		}
		del.Nack(false, false)
		return fmt.Errorf("job %s: %w", msg.ID, err)
	}
	del.Ack(false)
	return nil
}

// LocationWorkers owns one LocationWorker per location queue — the
// worker set cmd/worker starts, one Go worker per queue.
type LocationWorkers struct {
	conn    *Conn
	queues  []string
	workers map[string]*LocationWorker
}

// NewLocationWorkers builds one worker per queue in queues; an empty list
// means all of LocationQueues.
func NewLocationWorkers(conn *Conn, handler Handler, queues ...string) *LocationWorkers {
	if len(queues) == 0 {
		queues = LocationQueues
	}
	lw := &LocationWorkers{conn: conn, queues: queues, workers: make(map[string]*LocationWorker, len(queues))}
	for _, q := range queues {
		lw.workers[q] = NewLocationWorker(conn, q, handler)
	}
	return lw
}

// Start starts every worker.
func (lw *LocationWorkers) Start() error {
	for _, q := range lw.queues {
		if err := lw.workers[q].Start(); err != nil {
			return err
		}
	}
	return nil
}

// Pause pauses the worker for one queue (an unknown queue is a no-op).
// The game watchdog calls this with JobsGPUQueue.
func (lw *LocationWorkers) Pause(queue string) {
	if w, ok := lw.workers[queue]; ok {
		w.Pause()
	}
}

// Resume un-pauses the worker for one queue (an unknown queue is a no-op).
func (lw *LocationWorkers) Resume(queue string) {
	if w, ok := lw.workers[queue]; ok {
		w.Resume()
	}
}

// IsPaused reports whether the worker for one queue is paused.
func (lw *LocationWorkers) IsPaused(queue string) bool {
	if w, ok := lw.workers[queue]; ok {
		return w.IsPaused()
	}
	return false
}

// Stop stops every worker.
func (lw *LocationWorkers) Stop() {
	for _, q := range lw.queues {
		lw.workers[q].Stop()
	}
}
