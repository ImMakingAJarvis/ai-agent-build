// sched-audit is the subscriber on the weekly cadence queue
// (docs/scheduled-work.md). On each sched.weekly.tick it enqueues one
// agentjob — a brain round (class background, driver local) that audits every
// note and file the brain uses and writes a report: what is still needed, what
// could use changing, and what should be removed. It does not call the model
// itself: enqueuing a job is the established way to have the brain do work,
// so the existing agent-run loop runs the round.
//
// The audit job is idempotent per week — its id embeds the anchor-day date
// (e.g. audit-weekly-2026-11-09) — so a redelivered tick, or a manual fire
// landing in the same week, does not queue a second audit.
//
// Modes:
//
//	sched-audit         consume sched.weekly forever (the Docker default)
//	sched-audit once    consume at most one tick, process it, exit (manual / test)
//	sched-audit fire    enqueue this week's audit immediately (no tick) and exit
//
// Configuration (env):
//
//	RABBITMQ_URL    the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	JARVIS_DATA_DIR the agentjob store. Default ~/.local/share/jarvis.
//	SCHED_WEEK_DAY  the weekly anchor day, 0-6 (Sun=0) or a name. Default Monday.
//	TZ              set in compose; the tick's week key and report date use the
//	                container's local zone so they match the scheduler's boundary.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/sched"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// connectWithRetry dials the broker, backing off until it is reachable (the
// compose stack may bring rabbitmq up a little after sched-audit).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("sched-audit: broker not ready: %v", err)
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

// auditJobSpec builds the brain's audit job for the tick at time at. The week
// key (the anchor-day date, in the container's local zone) makes the id stable
// per week; the report date gives the brain a deterministic output path.
func auditJobSpec(at time.Time, anchor time.Weekday) agentjob.Spec {
	local := at.In(time.Local)
	weekKey := sched.WeekKey(local, anchor)
	return agentjob.Spec{
		ID:           "audit-weekly-" + weekKey,
		Title:        "Weekly notes & files audit (week of " + weekKey + ")",
		Instructions: auditInstructions(weekKey, local.Format("2006-01-02")),
		Driver:       "local",
		Class:        agentjob.ClassBackground,
		Priority:     1,
		CreatedAt:    at.UTC(),
		Source:       "sched-audit",
	}
}

func auditInstructions(weekKey, reportDate string) string {
	return fmt.Sprintf(`You are running the scheduled WEEKLY AUDIT (week of %s) of the notes and files the Jarvis brain relies on.
This is a read-only audit: inspect and report. Do NOT edit, move, or delete any audited file — the report only proposes changes; a later job or Nathan applies them.

Read the brain's own material (partial-read large files, stay within context):
- notes/agent/done.md, notes/agent/system.md, notes/agent/asked.md (the standing notes)
- the most recent ~10 notes/conversations/agent-*.md sections (recent rounds)
- CLAUDE.md (repo root) and ~/.claude/CLAUDE.md (the agent's own standing instructions)
- the memory dir's MEMORY.md and the memory files it points to
- docs/ — skim the index (ls docs/); read only a runbook that looks stale or contradicted
- scripts/jarvis-helper-context.md (the iGPU helper's setup brief)

Audit it all against what the stack actually looks like now. For each file/section decide one of: still needed, could use changing, should be removed. Watch specifically for: stale ports/paths/service names, facts the code has since changed, duplicated or contradictory notes, dead pointers (files or commands that no longer exist), and topics Nathan stopped asking about.

Write the report to notes/audits/audit-%s.md with exactly three sections:
  ## Still needed
  ## Could use changing
  ## Should be removed
Under each, one line per item: the file/section, then the concrete evidence and why it is in that bucket. Keep it focused — no filler.

When done:
- jarvis-discord notify with a one-paragraph summary of the three buckets and the report path.
- Do not open a PR for this: the report is the deliverable and it lives under notes/.`, weekKey, reportDate)
}

// handleTick processes one weekly tick: decode it, build the audit job for its
// week, and ingest it (idempotent). It returns an error only for a malformed
// tick or a store failure — the caller nacks those without requeue so they land
// on the dead queue rather than looping.
func handleTick(body []byte, anchor time.Weekday, store *agentjob.Store) error {
	e, err := queue.DecodeEvent(body)
	if err != nil {
		return fmt.Errorf("decode tick: %w", err)
	}
	if e.Kind != "" && e.Kind != queue.SchedKind {
		return fmt.Errorf("unexpected kind %q on %s (want %q)", e.Kind, sched.QueueName(sched.Weekly), queue.SchedKind)
	}
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	sp := auditJobSpec(at, anchor)
	added, err := store.Ingest(sp)
	if err != nil {
		return fmt.Errorf("ingest %s: %w", sp.ID, err)
	}
	if !added {
		log.Printf("sched-audit: %s already queued — duplicate tick, skipping", sp.ID)
		return nil
	}
	log.Printf("sched-audit: enqueued %s (%s)", sp.ID, sp.Title)
	return nil
}

// consumeOnce opens one channel and consumes the weekly queue until ctx is
// done, a single tick is handled in once mode, or the delivery channel closes
// (the broker connection dropped — the caller reconnects).
func consumeOnce(ctx context.Context, conn *queue.Conn, anchor time.Weekday, store *agentjob.Store, once bool) error {
	ch, err := conn.NewChannel()
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	defer ch.Close()
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("qos: %w", err)
	}
	name := sched.QueueName(sched.Weekly)
	dels, err := ch.Consume(name, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", name, err)
	}
	log.Printf("sched-audit: consuming %s (prefetch=1, manual ack)", name)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case del, ok := <-dels:
			if !ok {
				return fmt.Errorf("delivery channel closed (broker connection dropped)")
			}
			if err := handleTick(del.Body, anchor, store); err != nil {
				del.Nack(false, false) // do not requeue — routes to the dead queue
				log.Printf("sched-audit: %v", err)
			} else {
				del.Ack(false)
			}
			if once {
				return nil
			}
		}
	}
}

// consumeLoop wraps consumeOnce to ride out broker restarts: on any non-shutdown
// error it reconnects, re-ensures the queue/binding, and re-consumes. With
// restart: always, a permanent broker loss still recovers on the next boot.
func consumeLoop(ctx context.Context, conn *queue.Conn, anchor time.Weekday, store *agentjob.Store, once bool) error {
	for {
		err := consumeOnce(ctx, conn, anchor, store, once)
		if err == nil || ctx.Err() != nil {
			return err
		}
		log.Printf("sched-audit: %v — reconnecting", err)
		if rerr := conn.Reconnect(); rerr != nil {
			log.Printf("sched-audit: reconnect: %v", rerr)
		}
		if rerr := conn.EnsureSchedQueue(sched.Weekly); rerr != nil {
			log.Printf("sched-audit: re-ensure queue after reconnect: %v", rerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// fireAudit enqueues this week's audit immediately (no tick) — used by the
// `fire` mode and to test the enqueue path without waiting for a boundary.
func fireAudit(anchor time.Weekday, store *agentjob.Store) error {
	sp := auditJobSpec(time.Now().UTC(), anchor)
	added, err := store.Ingest(sp)
	if err != nil {
		return fmt.Errorf("ingest %s: %w", sp.ID, err)
	}
	if !added {
		log.Printf("sched-audit: %s already queued — nothing to do", sp.ID)
		return nil
	}
	log.Printf("sched-audit: enqueued %s (%s)", sp.ID, sp.Title)
	return nil
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	anchor, err := sched.AnchorWeekday(envOr("SCHED_WEEK_DAY", "1"))
	if err != nil {
		log.Fatalf("sched-audit: %v", err)
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		log.Fatalf("sched-audit: open job store %s: %v", agentjob.DataDir(), err)
	}

	// fire: enqueue now, no broker needed.
	if len(os.Args) >= 2 && os.Args[1] == "fire" {
		if err := fireAudit(anchor, store); err != nil {
			log.Fatalf("sched-audit: fire: %v", err)
		}
		return
	}
	// once: consume at most one tick, then exit.
	once := len(os.Args) >= 2 && os.Args[1] == "once"
	if len(os.Args) > 1 && !once {
		log.Fatalf("sched-audit: unknown argument %q (want no args, once, or fire)", os.Args[1])
	}

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("sched-audit: connect to broker: %v", err)
	}
	defer conn.Close()
	if err := conn.EnsureSchedQueue(sched.Weekly); err != nil {
		log.Fatalf("sched-audit: ensure %s: %v", sched.QueueName(sched.Weekly), err)
	}

	if err := consumeLoop(ctx, conn, anchor, store, once); err != nil && err != context.Canceled {
		log.Fatalf("sched-audit: run: %v", err)
	}
	log.Printf("sched-audit: stopped")
}
