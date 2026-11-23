# RabbitMQ → Kafka: investigation and plan

Status: **plan, not started** — 2026-11-21 (job chat-1795252621-71aa).
The switch runs in shadow mode first; nothing on the live AMQP path
changes until a queue family is deliberately cut over, one at a time.

## 1. What we want

Nathan's ask: switch the broker from RabbitMQ to Kafka, and build tools
that let us **see inside the queues, modify them, and do all kinds of
things** to them. Shadow mode before the full switchover.

What Kafka actually buys us (why it is worth doing):

- **A queue is a log, not a conveyor.** Messages stay after they are
  consumed (retention), so "what did the queue hold at 03:00?" is
  answerable, and a fixed worker can be re-run against the same
  messages. In AMQP, once a message is acked it is gone; `peek` today
  is a fetch-unacked / nack-requeue trick that can interleave with a
  live consumer.
- **Offsets are inspectable, seekable state.** Per-consumer progress is
  visible and rewound with a command (`seek`), which is the core of
  "modify the queue" — replay, skip, re-deliver.
- **Reprocessing for free.** A new consumer group starts at any offset;
  shadow consumers, auditors and the dashboard history view all read
  the same log without disturbing the real consumer.
- **History for the events family.** Lifecycle events are fire-and-
  forget on AMQP; on Kafka they become a durable 30-day log the
  dashboard can browse.
- **Standard tooling** (kcat, Admin API, exporters) plus a path to
  partitioned, parallel consumption later (worker queues are
  single-consumer today; that stays true until we choose otherwise).

What we lose or must rebuild (the real work):

| AMQP feature | Where it is used | Kafka answer |
|---|---|---|
| `x-max-priority` | worker queues (voice p10, image p10, coding p5, background p10 for `plan_epic`) | per-priority topics, consumer polls high→low (§3.2) |
| DLX + `x-death`, `MaxAttempts` | every `flow.*`/`cap.*` → `dead` | `dead` topic + janitor that routes failures/expiries (§3.3) |
| per-message TTL | `flow.*`/`cap.*` expiry | janitor compares envelope `deadline` (§3.3) |
| RPC via `amq.rabbitmq.reply-to` | `jarvis-mq call` | `rpc.request`/`rpc.reply` topics + corr filter (§3.4) |
| on-demand queues | `flow.*`, `cap.*`, `agent.*.inbox` | auto-created topics + janitor deletes stale ones (§3.5) |
| management API 15672 | `jarvis-mq queues/depth/purge`, dashboard queue page | Kafka Admin API via the new `jarvis-kq` CLI + dashboard repoint (§4) |
| peek (fetch-unacked) | `jarvis-mq peek`, dashboard | log read at any offset, no consumer involvement (§4) |
| **MQTT on 1883** | `jarvis-ha` (the only MQTT user) | decouple: mosquitto container (§6, phase 4) |

## 2. Current state (what has to keep working)

Topology (ground truth: `rabbitmq/definitions.json` + on-demand
declares in `internal/queue`):

- **Exchanges:** `tasks` (topic), `events` (topic), `jarvis.dlx`
  (fanout → `dead`).
- **Static queues (17):** `voice`/`fix`/`image`/`coding`/
  `background` (worker; priority 10/—/10/5/10), `review.{sonnet,
  opus,fable,local}`, `jobs.{gpu,cpu,igpu}` (task-runner; the
  game-pause gate pauses `jobs.gpu`), `sched.{hourly,daily,weekly,
  monthly}` (bound to `events`), `dead`.
- **On-demand queues:** `flow.<name>` (round handoffs), `cap.<cap>`
  (capability requests, envelope v2), `agent.<name>.inbox`
  (mailboxes) — each declared with
  `x-dead-letter-exchange=jarvis.dlx`.
- **Semantics in use:** priority-within-queue; manual ack, prefetch 1;
  `peek` = fetch-unacked then nack-requeue in reverse; dead letters
  carry `x-death` (reason, original queue, count, time); the
  dashboard's Dead Letters page lists/requeues/drops them;
  `jarvis-mq call` is sync RPC.
- **Users:** ~30 Go packages sit on `internal/queue` (worker,
  task-runner, plane-bridge, resourced, switchboard, jarvis-chat,
  jarvis-jobs, jarvis-photo, jarvis-mq, sched-tick/audit, wake-queue,
  review-worker/rev-seed/review-sweep, capworker-remote and the six
  capability workers, enqueue, dashboard, jarvis-migrate).
- **Not broker-bound (no change):** the agent-job store
  (`jarvis-jobs`, files under `~/.local/share/jarvis/jobs`), the
  switchboard listener store (JSON files), `notes/`.

**Key finding:** all AMQP knowledge is concentrated in one package —
`internal/queue` (~5,900 lines) — with `cmd/jarvis-mq` on top of it.
The switch is therefore: a Kafka implementation of the same surface +
the topology mapping above + the tooling. No service outside
`internal/queue` learns anything about Kafka.

## 3. Kafka design

### 3.1 Broker

- Single node, **KRaft** (no Zookeeper), compose service `kafka`
  (`bitnamilegacy/kafka:3.7` — Bitnami moved its Docker Hub org in
  2025 and the `bitnami/kafka` name no longer resolves), named volume
  `kafka_data`. ~1–2 GB JVM; the host has 46 GB RAM free.
- **Two client listeners** — a Kafka client follows the *advertised*
  host for partition leaders and group queries, so one name cannot
  serve both sides of the compose boundary:
  - `EXTERNAL` on `127.0.0.1:9092`, advertised as `127.0.0.1:9092`
    (host clients: `jarvis-kq`, the Go live tests);
  - `PLAINTEXT` on `kafka:9093`, advertised as `kafka:9093`
    (in-container clients: `kafka-init`, `kafka-exporter`).
  The KRaft controller listens on `9094` (quorum voters
  `0@kafka:9094`); internal, never a client path. A client on the wrong
  side of the boundary succeeds on bootstrap metadata, then fails on
  leader and group dials.
- `delete.topic.enable=true`, `auto.create.topics.enable=true`
  (constrained by the naming convention in §3.5).
- Prometheus metrics come from `kafka-exporter`, a small in-repo Go
  service (`cmd/kafka-exporter`, :9308) — the usual exporter images are
  pull-denied on this box.
- Retention: worker/review/jobs/sched/mailbox topics 7 days,
  `flow.*`/`cap.*` 7 days, `dead` 30 days, `events` 30 days.
  `min.insync.replicas=1` (single node).

### 3.2 Priority → per-priority topics

Kafka has no message priority. Each AMQP queue with a non-zero
`x-max-priority` becomes a **family of topics, one per priority class
that is actually used**:

| AMQP queue | Kafka topics | Consumer poll order |
|---|---|---|
| `voice` (max 10) | `voice.p10`, `voice` | `voice.p10` → `voice` |
| `image` (max 10) | `image.p10`, `image` | `image.p10` → `image` |
| `coding` (max 5) | `coding.p5`, `coding` | `coding.p5` → `coding` |
| `background` (max 10) | `background.p10`, `background` | `background.p10` → `background` |
| `fix` (none) | `fix` | `fix` |

The bare name is the default priority; `.<pN>` suffixes are the
higher classes. The publisher routes on the envelope's priority field;
the consumer polls high→low with prefetch 1, so "highest available
first" is preserved without any in-memory reordering. Envelope v2
already carries `priority` — the routing is a publish-time decision.

### 3.3 Dead letters and TTL → `dead` topic + janitor

- One topic `dead` (same name as the AMQP queue). A message that is
  rejected after the `MaxAttempts` budget (consumer-side count, in an
  `x-attempts` header carried in the Kafka record headers) or that
  passes its deadline is published to `dead` with headers
  `x-death-reason` (rejected|expired), `x-death-queue` (original
  topic), `x-death-attempts`, `x-death-time`. The dashboard's Dead
  Letters section and `jarvis-kq dead*` read this topic; requeue
  republishes to `x-death-queue` (recreated if gone — same rule as
  today's `RequeueDead`).
- **Janitor:** a small consumer (lives in the switchboard container —
  it already owns the flow-queue wake logic and the listener store)
  scans `flow.*`/`cap.*` topics for records whose envelope
  `deadline` has passed and routes them to `dead` with reason
  `expired`. Kafka has no per-message TTL; the envelope already
  carries the deadline, so this is a comparison, not new state.

### 3.4 RPC → `rpc.request` / `rpc.reply`

`jarvis-mq call` today publishes with
`reply-to=amq.rabbitmq.reply-to` and blocks. On Kafka: publish the
envelope to `rpc.request` with a unique `correlation_id`, then read
`rpc.reply` (a temporary consumer group, filtered on the corr id)
until the Result or the timeout. One topic pair serves all callers;
at this scale the corr filter is trivial.

### 3.5 On-demand topics and the naming convention

`flow.*`, `cap.*` and `agent.*.inbox` map to same-named topics,
auto-created on first publish. Conventions that keep the namespace
honest: topic names are lowercase `[a-z0-9.-]`, families are
`flow.`, `cap.`, `agent.*.inbox`, plus the static set in §2. The
janitor deletes a family topic that has had no records and no
consumers for 7 days (the AMQP equivalent is "delete by hand"; the
janitor is the new default).

### 3.6 Consumer groups

One group per consumer role: `worker`, `task-runner`, `switchboard`,
`review-worker`, `cap.<cap>`, `sched-audit`, `resourced`,
`jarvis-ha`-none (MQTT, §6). Offsets are committed on ack — the AMQP
ack model maps 1:1. Single partition per topic (order preserved,
single consumer — same as today); partitioning a topic later does not
touch the code, only the topic config.

## 4. The tools — `jarvis-kq`

A new Go CLI (`cmd/jarvis-kq`, client `segmentio/kafka-go`), the
queue-console for the new broker. It supersedes `jarvis-mq`'s
broker-facing commands once cutover completes; until then both exist.

**See inside (the log semantics — the main win):**

```
jarvis-kq topics                     # every topic: depth, min/max offset,
                                     # first/last record age, retention
jarvis-kq depth <topic>
jarvis-kq peek <topic> [-n N] [--offset off | --tail N]
                                     # read any position, no consumer involved
jarvis-kq tail <topic>               # follow new records
jarvis-kq groups                     # consumer groups + committed offsets
jarvis-kq lag                        # lag per group/topic/partition
jarvis-kq history <topic> [--since 2h]  # what flowed through, newest last
jarvis-kq describe <topic>           # config, partitions, offsets, times
```

**Modify:**

```
jarvis-kq pop <topic> [-n N]         # consume + commit (consume equivalent)
jarvis-kq seek <group> <topic> [--to off|--to ts|--earliest|--latest]
                                     # rewind/advance a consumer's position
jarvis-kq replay <topic> --from <off|ts> --to <topic|group>
                                     # re-deliver a slice of the log
                                     # (fix a worker, re-run its inputs)
jarvis-kq requeue <topic> <offset|corr>   # re-publish one record to the
                                     # front of its (original) topic
jarvis-kq edit <topic> <offset> --set k=v # republish a record with an
                                     # edited payload; the original stays in
                                     # history (the log is append-only —
                                     # "edit" means supersede)
jarvis-kq purge <topic>              # truncate to the end (deleteRecords)
jarvis-kq drop <topic>               # delete the topic
jarvis-kq send <topic> [--payload k=v ...] [--priority N]
                                     # publish (test messages, manual wakes)
```

**Dead letters (same operations as the dashboard buttons, from the CLI):**

```
jarvis-kq dead                       # list dead records + original topic
jarvis-kq dead-requeue <id>          # back to the original topic
jarvis-kq dead-drop <id>
```

**Consumer control (via a `kq.control` topic the consumer SDK
honors, so it works across restarts):**

```
jarvis-kq pause <group> [--topic <topic>]
jarvis-kq resume <group>
jarvis-kq throttle <group> --rate <n>/s
jarvis-kq rebalance <group>          # Admin API
```

**Dashboard:** the queues page re-points from the 15672 scrape to the
Kafka Admin API (topics/depth/lag); Dead Letters reads `dead`; a new
per-topic history table (recent records, click-through to payload).
Prometheus gains `kafka_exporter` in place of the rabbitmq exporter.
Raw escape hatch: `docker exec` + `kcat` in the kafka container.

*As built (kafka-tools-1):* the modify side landed as planned, with the
dead-letter mechanics spelled out. `dead` records carry their original
queue/reason/attempts in `x-death-*` headers; requeue republishes to the
original queue with a `kq-requeued` marker header, and requeue/drop each
append a marker to `kq.dead.dropped` — a record is hidden from `dead`
and the dashboard once marked (`--all` shows it). Control
(pause/resume/throttle) writes `kq.control` records (key = group) — a
log, so it sticks across restarts; `rebalance` is the one Admin API call
in the set. During the dual-run the queues page shows the Kafka tile
*alongside* the AMQP queues — the re-point completes when the shadow
gate passes, not before. Prometheus gained the in-repo
`kafka-exporter` job (`jarvis-kafka-exporter:9308`); the rabbitmq job
stays until cutover. One client bug worth keeping: a bounded read with a
zero-MaxWait fetch blocks until the conn deadline (30 s), past the
caller's context, so the next dial in the sequence failed with a fake
"lookup … i/o timeout" — `kq.ReadRange` now derives its deadline from
ctx and fetches with MaxWait 1 s (live-verified: ListDead 58 s → 2 s).

## 5. Shadow mode (the safe path)

**Phase 0 — bring-up.** `kafka` compose service + `specs/kafka.md`.
`jarvis-kq` v1 (the read commands + `send`/`purge`/`drop`). Nothing on
the AMQP path changes. Gate: `jarvis-kq topics` sane, a `send` +
`peek` round-trip works, exporter metrics land in Prometheus.

**Phase 1 — shadow publish (dual-write).** A mirror queue (AMQP)
bound `#` on both `tasks` and `events` feeds a small mirror consumer
(`kshadow`, can share the switchboard container) that writes every
message to the same-named Kafka topic with header `source=amqp`. No
publisher changes — the exchange fan-out is the tap. The mirror also
records a per-queue **watermark** (last mirrored message id, on topic
`kq.watermark`) — used by rollback (§7). A parity report (dashboard
tile + `jarvis-kq parity`): mirrored count vs consumed count per
queue, sample payload diff, shadow lag. Gate: 48 h, zero mismatch,
lag bounded.

*As built (kafka-shadow-1):* `kshadow` is its own compose service
(`specs/kshadow.md`), not the switchboard container — a mirror bug
should never take the switchboard down. Attribution is by routing-key
shape (`internal/kshadow/attribution.go`): task-family keys and
`*.inbox` go to a topic named after the queue, `sched.<cadence>.*` go
to the single `events` topic with the routing key in a header. Message
ids: the AMQP id when set, else the JSON `id`/`correlation_id`/
`job_id` field, else a hash of the body. Watermarks throttle to one
write per 5 s per queue, with a final flush on shutdown (rollback must
never see a stale id). A write that fails 6×5 s is Nacked and requeued
— `kq.mirror` has no DLX by design, so an un-mirrored message waits on
the mirror queue instead of leaking into `dead`. Parity reads kshadow's
state file (bind-mounted `/data`) plus the management API (baselines
captured on first sight of each queue) plus an independent scan of the
`source=amqp` records — three independent counts, so a gap between any
two localizes the leak. `jarvis-kq parity` exits 0/1/2/3 for
ok/mismatch-or-dropped/lag>5m/state-unreadable.

**Phase 2 — shadow consume (dry-run).** `kshadow` gains a
dry-run consumer per family: it reads the mirrored Kafka topics and
logs what it *would* do (deliver to worker, wake job X, run cap Y),
compared against what the AMQP side actually did (the `events`
exchange already records `job.*` lifecycle events). Findings go to
the parity report. This is where the priority routing (§3.2) and the
janitor (§3.3) get proven against live traffic without touching the
live path. Gate: 7 days, every "would do" matches a real event.

*As built (kafka-cutover):* the dry-run runs inside the kshadow
container (`internal/kshadow/dryrun.go`) with its own state file
(`kshadow-dryrun.json` next to the mirror's). Fresh readers start at
log end (pre-gate test traffic does not count); topics that appear
mid-gate and persisted offsets start at log start/persisted offset.
Match modes per family: `event` (cap/jobs/worker — a `job.*` event with
the envelope's id), `agent` (flow/mailboxes — an `agent.<job-id>.*`
event; flow's job id comes from the topic name, mailboxes' from
`wake_job`), `depth` (events/sched/review — well-formedness only). The
worker family mirrors the real worker: one lane per base queue
(voice/fix/image/coding/background), prefetch 1, polling
`[X.pN, X]` in order, with inversions counted within a 5-minute
concurrency window. The janitor sweep (30s) counts would-expire entries
and compares dead-queue arrivals (mgmt counters minus the first-sweep
baseline) against them. `jarvis-kq parity` exits 1 when any family has
an unmatched would-do or an inversion; the dashboard tile shows the
gate line. Dedupe by message id (§7.2) makes restarts and replays
idempotent.

**Phase 3 — per-family cut-over.** Each family flips with an env
switch (`KQUEUE_<FAMILY>=on|off`, default off) read by
`internal/queue`'s consumer and publisher. Order, lowest risk first:

1. `events` — fire-and-forget; nothing's work depends on it.
2. `sched.*` — small ticks; verify at the next boundary crossing.
3. `review.*` — low traffic, single consumer.
4. `agent.*.inbox` — mailboxes; the switchboard is the consumer.
5. `cap.*` — capability requests; verify with `jarvis-mq call`.
6. `flow.*` — round handoffs; low volume, fully agent-driven,
   already proven in phase 2.
7. `jobs.*` — placement; the game-pause gate must keep working.
8. `voice`/`fix`/`image`/`coding`/`background` — the worker; last.

Per family: publishers dual-write (AMQP + Kafka), consumers flip to
Kafka, watch ≥24 h and a meaningful message count, then stop that
family's AMQP consumer. The switch flips the family back in seconds.

**Phase 4 — cleanup.** MQTT decoupled (§6), `rabbitmq` retired
(volume backed up first), `internal/queue`'s AMQP implementation
frozen/removed, mirror queue and watermark topics deleted.

## 6. MQTT (the one RabbitMQ feature Kafka does not have)

Only `jarvis-ha` uses MQTT (auto-discovered sensors; the LWT that
marks entities unavailable). It is independent of the queue work:

- **During phases 0–3** RabbitMQ keeps running as-is (MQTT included).
- **Phase 4:** a small `mosquitto` container takes over 1883
  (`jarvis-ha`'s `HA_MQTT_URL` repoints); the `rabbitmq` container and
  its volume are then retired. (Alternative considered: HA REST/WS
  instead of MQTT — rejected, auto-discovery + LWT are exactly why
  MQTT is there.) This also removes the 2026-11-04 failure mode (the
  0-byte MQTT dets file that needed a human).

## 7. Rollback and the duplicate-copy problem

Dual-write means every message exists in **both** brokers while a
family is in flight. If the Kafka consumer has already consumed and
acted on a message, the AMQP copy is still sitting in its queue —
flipping the family back would make the AMQP consumer reprocess it.
Two safeguards, both in the plan from the start:

1. **Watermark purge on rollback.** The phase-1 mirror records the
   last-mirrored message id per queue (`kq.watermark`). `jarvis-kq
   rollback <family>` purges the AMQP queue up to that watermark
   before the switch flips back.
2. **Idempotence by message id.** The worker already uses the message
   id as the agent-job id, and the agent-job store treats a re-add of
   an existing id as a no-op; the Kafka consumer must honour the same
   rule for every family (a consumer that re-sees an id it already
   acked skips it). This is what makes `seek`/`replay` safe too.

*As built (kafka-tools-1):* `jarvis-kq rollback <queue> [--dry-run]`
implements safeguard 1. The boundary is kshadow's state file (per-
message `last_id`), falling back to the newest `kq.watermark` record.
Popping with `reject_requeue_false` drops up to and including the
boundary; if the queue drains first, the boundary was never mirrored
and every popped message is republished to its original exchange (the
mgmt API's publish takes base64 only) with exit 3. `--dry-run` pops one
message with requeue — requeue puts it back at the *head* of the queue,
so one pop is all a dry run can show — and classifies the oldest
message. Live-verified on a test queue: boundary-deeper and
oldest-is-boundary (both dry + real); the exit-3 path is by inspection.

Whole-broker rollback (Kafka itself is dead): flip every family off —
AMQP is still the source of truth for families not yet cut over, and
for cut-over families the watermark purge (§7.1) has kept the AMQP
queue in step.

## 8. Risks and open questions

- **Priority routing** is the only semantic with real behaviour
  change. Mitigation: the existing live tests
  (`internal/queue/*_live_test.go`) gain Kafka twins in phase 0, and
  phase 2 dry-run watches the worker's actual pick order.
- **TTL janitor** is a new moving part. Open: verify in phase 2 that
  nothing relies on broker-side TTL expiry that the envelope
  `deadline` does not already express (the switchboard's wake
  deadlines are the other half of expiry today).
- **Topic growth** from per-round `flow.*`: bounded by the 7-day
  inactivity delete; the broker handles thousands of topics.
- **The 2026-11-04 MQTT incident** stays relevant until phase 4
  (RabbitMQ still runs the MQTT plugin) — the watchdog rule stands.
- **Dashboard + Prometheus** re-pointing is a chunk of the tools
  work; it is scoped in §4 so it is not forgotten.

## 9. Follow-up jobs (queued with this plan)

1. **kafka-shadow-0** — phase 0: `kafka` compose service + spec,
   `jarvis-kq` v1 (read + send/purge/drop), live tests for the
   priority routing.
2. **kafka-shadow-1** — phase 1: mirror queue + `kshadow` dual-write
   + watermark + parity report.
3. **kafka-tools-1** — `jarvis-kq` modify commands (seek/replay/
   requeue/edit/pause/throttle/rollback) + dashboard re-point
   (queues page, dead letters, history) + `kafka_exporter`.
4. **kafka-cutover** — phases 2–3, family by family in §5 order.
   *First step: ask on Discord whether the plan is approved before
   touching any consumer.*
5. **kafka-mqtt** — phase 4: mosquitto, `jarvis-ha` repoint,
   `rabbitmq` retirement.
