// sched-tick is the cron publisher behind the periodic work queues
// (docs/scheduled-work.md). It polls the wall clock and, each time a
// cadence boundary is crossed, fires one sched.<cadence>.tick event onto the
// events exchange, which routes it to the durable sched.<cadence> queue. Any
// service may subscribe to a cadence queue to run scheduled work on the hour,
// day, week or month.
//
// Cadences and their boundaries (in the container's timezone — set TZ in
// compose; the brain's notes are local-dated, so default to the host zone):
//
//	hourly   every hour at :00
//	daily    midnight (the day rolls over)
//	weekly   midnight of SCHED_WEEK_DAY (default Monday)
//	monthly  midnight of the 1st
//
// It does not catch up: a boundary crossed while the process was down is not
// replayed on restart (the next boundary still fires) — the same rule cron
// uses. Fire-and-forget: a tick that cannot be published within the window is
// dropped with a log line, never retried forever.
//
// Modes:
//
//	sched-tick            run the tick loop forever (the Docker default)
//	sched-tick fire CAD   publish one tick to CAD now and exit (manual / test)
//
// Configuration (env):
//
//	RABBITMQ_URL    the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	SCHED_POLL      how often the clock is checked. Default 30s.
//	SCHED_WEEK_DAY  the weekly anchor day, 0-6 (Sun=0) or a name. Default Monday.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/sched"
)

const (
	defaultPoll      = 30 * time.Second
	publishGiveUp    = 30 * time.Second // how long one tick's publish is retried
	publishRetryWait = 2 * time.Second
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("sched-tick: bad %s %q: %v", key, v, err)
	}
	return d
}

// connectWithRetry dials the broker, backing off until it is reachable (the
// compose stack may bring rabbitmq up a little after sched-tick).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("sched-tick: broker not ready: %v", err)
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

// publishTick fires one tick, retrying for up to publishGiveUp to ride out a
// short broker restart; on a hard failure it logs and gives up (the next
// boundary still fires — a lost tick is acceptable, per the events-exchange
// contract).
func publishTick(ctx context.Context, events *queue.EventPublisher, c sched.Cadence) {
	e := queue.SchedEvent(c)
	deadline := time.Now().Add(publishGiveUp)
	for {
		if err := events.Publish(ctx, e); err == nil {
			log.Printf("sched-tick: fired %s tick (at %s)", c, e.At.Format(time.RFC3339))
			return
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			log.Printf("sched-tick: %s tick not published within %s — dropped (the next boundary still fires)", c, publishGiveUp)
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("sched-tick: stopping before the %s tick was published", c)
			return
		case <-time.After(publishRetryWait):
		}
	}
}

func runLoop(ctx context.Context, events *queue.EventPublisher, anchor time.Weekday, poll time.Duration) error {
	prev := sched.Snapshot{}
	log.Printf("sched-tick: running (poll=%s, weekly anchor=%s); fires hourly/daily/weekly/monthly ticks", poll, anchor)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := time.Now()
			cur := sched.At(now, anchor)
			for _, c := range prev.Fired(cur) {
				publishTick(ctx, events, c)
			}
			prev = cur
		}
	}
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("sched-tick: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "sched-tick")
	if err != nil {
		log.Fatalf("sched-tick: event publisher: %v", err)
	}
	defer events.Close()

	// Manual / test mode: fire one tick and exit.
	if len(os.Args) >= 3 && os.Args[1] == "fire" {
		c := sched.Cadence(os.Args[2])
		if !sched.Known(c) {
			log.Fatalf("sched-tick: unknown cadence %q (want %v)", c, sched.All)
		}
		if err := conn.EnsureSchedQueue(c); err != nil {
			log.Fatalf("sched-tick: ensure %s queue: %v", c, err)
		}
		e := queue.SchedEvent(c)
		if err := events.Publish(ctx, e); err != nil {
			log.Fatalf("sched-tick: fire %s: %v", c, err)
		}
		log.Printf("sched-tick: fired %s tick (at %s)", c, e.At.Format(time.RFC3339))
		return
	}
	if len(os.Args) > 1 {
		log.Fatalf("sched-tick: unknown argument %q (want no args, or: fire CADENCE)", os.Args[1])
	}

	if err := conn.EnsureAllSchedQueues(); err != nil {
		log.Fatalf("sched-tick: ensure sched queues: %v", err)
	}
	anchor, err := sched.AnchorWeekday(envOr("SCHED_WEEK_DAY", "1"))
	if err != nil {
		log.Fatalf("sched-tick: %v", err)
	}
	if err := runLoop(ctx, events, anchor, envDuration("SCHED_POLL", defaultPoll)); err != nil && err != context.Canceled {
		log.Fatalf("sched-tick: run: %v", err)
	}
	log.Printf("sched-tick: stopped")
}
