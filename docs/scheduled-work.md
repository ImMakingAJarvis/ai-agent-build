# Scheduled work: cadence queues

The **cadence queues** let any part of the stack run work on a fixed wall-clock
cadence — on the hour, day, week, or month — without each consumer implementing
its own timer. A single publisher watches the clock and fires one event at each
boundary crossing; every cadence has its own durable queue that any number of
services may subscribe to.

```
                    ┌─────────────────────────────────────────────┐
                    │  events (topic exchange)                     │
                    │  routing key: sched.<cadence>.tick           │
   sched-tick ─────►│                                              │
   (polls clock)    └──┬──────────┬──────────┬──────────┬─────────┘
                       ▼          ▼          ▼          ▼
                 sched.hourly sched.daily sched.weekly sched.monthly
                                    │
                                    ▼
                                sched-audit
                          (enqueues a brain round
                           that audits the notes)
```

## Queues

Four durable queues, one per cadence, all on vhost `/`. Each is bound to the
`events` topic exchange under `sched.<cadence>.#`, and each carries the
dead-letter args so a tick a subscriber rejects (or that expires) lands on
`dead` with an `x-death` header instead of vanishing — the same convention as
`flow.*` and `cap.*` ([event-driven.md](event-driven.md#queues)).

| Queue | Fires at | Bound to | DLX |
|---|---|---|---|
| `sched.hourly` | every hour at `:00` | `events` on `sched.hourly.#` | `jarvis.dlx` |
| `sched.daily` | midnight (the day rolls over) | `events` on `sched.daily.#` | `jarvis.dlx` |
| `sched.weekly` | midnight of `SCHED_WEEK_DAY` (default Monday) | `events` on `sched.weekly.#` | `jarvis.dlx` |
| `sched.monthly` | midnight of the 1st | `events` on `sched.monthly.#` | `jarvis.dlx` |

A tick is a `sched.<cadence>.tick` event: `queue.Event{Kind: "sched",
Subject: <cadence>, State: "tick"}`, so its routing key is the standard three
segments and `ParseEventRoutingKey` reads it back with no special case.

The queues are declared in `rabbitmq/definitions.json` **and** created on demand
by `queue.EnsureSchedQueue` / `EnsureAllSchedQueues` (`internal/queue/sched.go`),
idempotently. That means a publish before the broker has reloaded
`definitions.json` still routes — the code path self-heals, exactly like the
`flow.*`/`cap.*` queues. The boundary math itself is pure and broker-free in
`internal/sched` (`At`, `Fired`, the `*Key` helpers), covered by
`internal/sched/sched_test.go`.

## The publisher: `sched-tick`

`cmd/sched-tick` (compose service `sched-tick`) is the cron. It polls the wall
clock every `SCHED_POLL` (default 30 s) and, each poll, compares the current
boundary keys against the previous poll's (`sched.Snapshot.Fired`). For every
cadence whose boundary was crossed it fires one tick to that cadence's queue.
At a shared boundary several fire at once — Monday 00:00 is also an hour, a day
and a week, so all three queues get a tick.

Two rules keep it cron-like:

- **No startup replay.** A fresh process records its first snapshot and fires
  nothing, so a restart does not replay every boundary since boot.
- **No catch-up.** A boundary crossed while the process was down is not
  replayed — the next boundary still fires. A tick that cannot be published
  within its window is dropped with a log line (the events-exchange
  fire-and-forget contract), never retried forever.

The cadence queues are created once at start (`EnsureAllSchedQueues`), so they
exist even before the broker reloads `definitions.json`.

Environment (set in `docker-compose.yml`):

| Var | Meaning | Default |
|---|---|---|
| `RABBITMQ_URL` | the broker | `amqp://jarvis:jarvis-local@localhost:5672` |
| `TZ` | the zone boundaries are computed in | `America/Los_Angeles` (matches the local-dated notes) |
| `SCHED_POLL` | clock poll interval | `30s` |
| `SCHED_WEEK_DAY` | the weekly anchor: cron's `0-6` (Sun=0) or a name | `1` (Monday) |

`SCHED_WEEK_DAY` must match on `sched-tick` and `sched-audit` — the publisher
uses it to decide *when* the weekly tick fires, and the subscriber uses it to
label the audit's week.

Manual / test mode: `sched-tick fire <cadence>` publishes one tick to that
cadence now and exits (used to exercise a subscriber without waiting for a
boundary).

## The subscriber: `sched-audit`

`cmd/sched-audit` (compose service `sched-audit`) is the first cadence
subscriber. It consumes `sched.weekly` (prefetch 1, manual ack) and, on each
tick, enqueues **one agentjob** — a brain round (class `background`, driver
`local`) that audits every note and file the brain uses and writes a report:
what is still needed, what could use changing, and what should be removed.

It does not call the model itself. Enqueuing a job is the established way to
have the brain do work, so the existing `agent-run` loop picks the job up and
runs the round — the audit is a normal brain round, not a side-channel model
call. The job's instructions (built by `auditInstructions`) point the brain at
the standing notes, recent conversation notes, both `CLAUDE.md` files, the
memory dir, the docs index and the helper brief, and have it write the report
to `notes/audits/audit-<local-date>.md` under the three buckets, then
Discord-notify a one-paragraph summary.

The job is **idempotent per week**: its id embeds the anchor-day date
(`audit-weekly-<week-key>`, e.g. `audit-weekly-2026-11-09`), computed from the
tick's local time. A redelivered tick, or a manual fire landing in the same
week, hits the existing job and is a no-op (`Store.Ingest` returns
`added=false`) rather than a duplicate audit. A malformed tick is nacked
without requeue and lands on `dead`.

Environment: `RABBITMQ_URL`, `JARVIS_DATA_DIR` (the agentjob store, default
`~/.local/share/jarvis`), `TZ`, and `SCHED_WEEK_DAY` (must match `sched-tick`).
Modes: `sched-audit` (consume forever — the Docker default), `sched-audit once`
(consume at most one tick and exit), `sched-audit fire` (enqueue this week's
audit now, no tick, no broker).

## Adding another cadence subscriber

To run more scheduled work, write a small consumer of a cadence queue — no
publisher change is needed, since every cadence already fires:

1. `queue.EnsureSchedQueue(<cadence>)` once at start (idempotent).
2. Open a channel, `Qos(1, 0, false)`, `Consume(sched.QueueName(<cadence>), "",
   false, ...)`, manual ack.
3. On each delivery, `queue.DecodeEvent` the body (the `sched.*` tick) and do
   the work; `ack` on success, `nack(false, false)` on final failure so it
   lands on `dead` instead of looping.

`cmd/sched-audit` is the reference implementation. Because the four queues are
durable and fire-and-forget, a subscriber that is down when a tick fires simply
finds the tick waiting in its queue on the next start — the cadence does not
lose a firing to a restart the way `sched-tick`'s no-catch-up rule would for a
missed boundary.

## Operations

```sh
# queue depths (0 when idle)
docker exec jarvis-rabbitmq rabbitmqctl list_queues name messages | grep sched.
# fire a tick to test a subscriber now
docker compose run --rm sched-tick fire weekly
# enqueue this week's audit without waiting for Monday
docker compose run --rm sched-audit fire
# where did a rejected tick go?
docker compose exec rabbitmq rabbitmqctl list_queues name messages | awk '$1=="dead"'
```
