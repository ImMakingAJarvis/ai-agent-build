# Event-driven work: queues and flow

The worker queues (voice/fix/image/coding/background, jobs.gpu/cpu/igpu)
move *jobs* from the scheduler to a model. **Flow queues** (`flow.<name>`)
are the event-driven channel between models *across rounds*: one round
publishes a job to a flow queue, finishes; another model does the work and
publishes the result back; the first model's next round consumes it and
continues. [Queues](#queues) is the full topology in one place.

## Queues

Everything lives on vhost `/`: three durable exchanges and nine queue
families. Every queue is durable; the only transient queues are the
temporary auto-delete ones the CLI creates to `tail` an exchange.

| Queue | Purpose | Owner — declares / consumes | Priority | DLX |
|---|---|---|---|---|
| `tasks` (topic) | the job exchange — every queue below binds it under `<name>.#` and receives at `<name>.default` | `definitions.json` + every `queue.Conn` (idempotent) | — | — |
| `voice` | voice conversations; must always get an answer (invariant 6) | worker (`internal/queue.Consumer`) | 10 | — |
| `fix` | hot-fix jobs; the scheduler drains it right after voice | worker | — | — |
| `image` | photo generation requests; the worker re-ingests them as `photo`-tagged agent jobs, so the swap window runs them as code (`docs/brain-swap.md`) | worker | 10 | — |
| `coding` | coding jobs to the model | worker | 5 | — |
| `background` | everything else; `plan_epic` jobs ride here at priority 10 (`PlanEpicPriority`) so plans queue ahead of the rest | worker | 10 | — |
| `jobs.gpu` / `jobs.cpu` / `jobs.igpu` | execution jobs placed by `internal/placement` from free VRAM/RAM | task-runner (LocationWorkers); publishers: plane-bridge (board `run_task` jobs) and the worker's LocationPublisher | — | — |
| `flow.<name>` | event-driven handoff between models across rounds (The pattern below); created on demand, one per flow | the round that created it — read with `jarvis-mq peek` / `consume` | — | `jarvis.dlx` |
| `cap.<capability>` | request channel for a capability — v2 messages, e.g. `cap.image.generate`; created on demand; the result goes to the request's `reply_to`, not here | the capability's worker (capworker SDK; `cap-echo` consumes `cap.echo`, the Phase 2 gate's test worker) | 10 | `jarvis.dlx` |
| `dead` | dead letters from `flow.*`/`cap.*`: rejected (nack without requeue after the `MaxAttempts` budget) or expired (TTL), each carrying an `x-death` header (reason, queue, count, time) | nobody consumes it — the dashboard's Dead Letters section reads it and offers requeue / drop | — | — |
| `events` (topic) | lifecycle events: `job.<capability>.<started\|progress\|done\|failed>`, `agent.<job-id>.<yielded\|woken\|resumed>`, `window.<opened\|closed>`, `lease.<granted\|released\|denied>` | publishers: worker (the jobs it runs), task-runner (`job.run_task.*`), agent-run (`agent.*`); declared by `definitions.json` + `EventPublisher` | — | — |
| `jarvis.dlx` (fanout) | the dead-letter exchange — fanout, so every dead letter lands on its single bound queue, `dead` | `EnsureDLX` (called from every `flow.*`/`cap.*` declare and dead-letter operation) | — | — |

Notes:

- **Worker queues** (`voice` … `background`) are declared, bound and
  consumed by the worker's consumer (prefetch 1, manual ack). They are in
  `definitions.json` so a broker restart re-creates them before the worker
  comes back; the worker's own declares are the same args, so the two
  coexist (a redeclare with different args would 406). Priority is the
  `x-max-priority` queue argument — within a queue, AMQP delivers
  higher-priority messages first. `fix` has none: it is a plain durable
  queue (EVB-11 added it to `definitions.json`; the worker has always
  declared it at runtime).
- **Location queues** (`jobs.*`) are in `definitions.json` and consumed by
  the task-runner; the game-pause gate pauses/resumes `jobs.gpu`.
- **`flow.*` and `cap.*` are never in `definitions.json`** — every round
  can spawn its own, so they are created on demand (declare + bind before
  publish) and deleted by hand. They alone carry the DLX argument
  (`x-dead-letter-exchange=jarvis.dlx`); the worker- and location-owned
  queues do not — a message the worker nacks there is dropped and logged,
  and the dead-letter guarantee (invariant 9) covers `flow.*` and `cap.*`.
- **`dead` is a mailbox, not a queue**: read get-style so listing never
  destroys a dead letter (`ListDead`), `RequeueDead` republishes to the
  queue named in `x-death` (recreated if it is gone), `DropDead` discards.
- **Events are fire-and-forget and never the source of truth** — a lost
  event is acceptable, so publishing one never blocks or fails the
  emitting job.

## The pattern

1. **Publish** the job to `flow.<name>` — the message carries the
   instructions for the target model *and* a `context` block left for the
   publisher's next round.
2. **Wake the target**: a flow queue has no worker of its own, so the
   publisher also adds a small agent job (or publishes `kind=agent` to a
   standard queue) telling the target model to consume `flow.<name>` and do
   what it says.
3. The target model **does the work**, then **publishes the result back to
   the same flow queue** and adds a small agent job that wakes the
   publisher's brain (that is what `flow.e2e-wake` messages are for).
4. The publisher's **next round consumes** the flow queue
   (`jarvis-mq consume flow.<name>`), reads the result plus the context it
   left itself, and continues.
5. **Tidy up**: `jarvis-mq delete flow.<name>` once the flow is finished
   (a durable empty queue is harmless, but deleting keeps the broker clean).

## Parking a round on a result (yield / waiting / wake)

The pattern above is the queue half; the agent-job store is the brain half.
A round that has published work (a `correlation_id`) and needs the result in
a LATER round does not finish — it **yields**: the job parks in the
`waiting` state, and the round's durable record, the **continuation**, is
written first.

- **`jarvis-jobs yield <id> --wait-on c1,c2 --note "..."`** — writes
  `jobs/<id>/continuation.md` (sections *Goal / Done so far / In flight
  (corr ids) / What to do with each result / Next steps / Gotchas*; the
  `--in-flight`/`--next`/`--results`/`--gotchas` flags fill them, a
  continuation the round wrote in its own words is merged in with the
  flags winning only the sections they name) and
  `jobs/<id>/continuation.json` (corr ids, deadline, round count, last
  driver, result refs), then flips the job to `waiting` via
  `Store.Yield` (deadline `--wait-until`, a duration or RFC3339, default
  2h). **The continuation is the precondition of the yield** (invariant
  8): the CLI reads the file back and refuses if "In flight" or "Next
  steps" is empty — the status is untouched, no `.json` is written, and
  the scaffold stays on disk to be filled in and retried.
- **`jarvis-jobs waiting`** — the waiting jobs: id, title, what it waits
  on, age, deadline. Prints nothing when none wait.
- **`jarvis-jobs wake <id>`** — the manual wake (the automatic one is the
  switchboard, plan task 14). It asks `Store.Wake`, which honours
  the same gate both paths share: the job wakes — back to `ready`,
  priority bumped above the ready peers — when every `--wait-on` result
  has been recorded or the deadline has passed; otherwise it stays
  waiting and the operator is told which corr is still missing.
  `jarvis-jobs requeue <id>` forces it ready regardless.

The **woken round reads `jobs/<id>/continuation.md` first** — what its
predecessor was doing, what it expected of each result, the exact next
step — and resumes instead of re-planning. `notes/brain-handoff.md` is a
pointer for the current window only (`docs/brain-swap.md`); the
continuations in the job store are the durable per-job record. When a
result is recorded (`Store.RecordResult`), the store keeps the
continuation's `result_refs` in step with `results.json`.

## Semantics

- Queues are **created on demand** (`PublishFlow` declares + binds before
  every publish; re-declaring with identical args is a no-op). They are not
  in `rabbitmq/definitions.json` because every round can spawn its own.
- Messages are **persistent**: a message published before any consumer
  exists sits on the broker until consumed.
- Flow queues are **not priority queues** — ordering is guaranteed by the
  single consumer (the next round), so no AMQP priority is sent.
- **Peek, then consume**: `peek` inspects without removing (fetch-unacked,
  then nack-requeue in reverse order to restore front-of-queue order); if
  the process dies between fetch and put-back the broker requeues the
  unacked messages, so nothing is lost. `consume` pops and acks.
- **Message IDs must be unique per publish** (the worker uses the id as the
  agent-job id when it ingests `kind=agent`). `jarvis-mq` defaults to
  `mq-<unix>-<6 random hex>`; the superagent tool uses `sa-<unix>-<suffix>`.
  Never reuse a fixed id.
- **Nothing disappears silently** (EVB-10): every `flow.*`/`cap.*` queue is
  declared with `x-dead-letter-exchange=jarvis.dlx`, so the broker itself
  routes a message to the durable `dead` queue when a consumer rejects it
  (nack without requeue — the delivery budget is `MaxAttempts`, the count
  rides in the `x-attempts` header) or when a per-message TTL expires.
  Each dead letter carries an `x-death` header (reason, original queue,
  time); the dashboard's Dead Letters section lists them and can requeue
  one back to its original queue (recreated if it is gone) or drop it.
  A message is either processed, dead-lettered with a reason, or requeued.

## Capability envelope (v2)

The v2 envelope (plan §3.3) is how a round or worker *requests a capability*
from another model and *receives its result*. Every field is `omitempty`, so a
v1 message still encodes byte-for-byte, and a v2 field that is unset stays out
of both the JSON and the AMQP properties.

- `capability` — dotted name of the work (`image.generate`, `voice.tts`, …);
  the worker for that capability picks the job up.
- `correlation_id` — the request's unique id; the result is keyed by it.
  **Mirrored onto the native AMQP `correlation_id` property** so the broker's
  logs, the CLI, and a Python worker see the same value the JSON body carries.
- `reply_to` — the queue (or flow) the result is published to. **Mirrored
  onto the native AMQP `reply_to` property.**
- `wake_job` / `parent_job` — the job to wake when the result lands / the job
  this one is part of.
- `input` (map) / `input_refs` (paths) — the request's arguments and any input
  files, **by path, never by bytes**.
- `out_dir` — where the worker writes its artifacts.
- `location` — `gpu` | `igpu` | `cpu` | `remote:<host>`.
- `resources` — `{vram_mb, ram_mb}` the request expects to need.
- `deadline` — when the request expires.
- `attempt` — which delivery this is (0 for the first).
- `v` — the envelope schema version, `2`.

The **result** is a small separate message (`queue.Result`) published to
`reply_to`, keyed by `correlation_id`:

- `status` — `ok` | `error` | `timeout` | `cancelled`.
- `artifacts` — **paths, never bytes** (results by reference, plan invariant 7).
  A result points at files on disk instead of carrying the bytes, so it stays
  small and any consumer can pull the actual bytes when it needs them.
- `summary`, `data` — a human line and structured extras.
- `worker`, `model` — who did the work.
- `started_at` / `finished_at`, `error`.

### Idempotency by correlation_id

A request may be delivered more than once (redelivery, requeue, a worker that
crashed after starting). Handlers are idempotent by **`correlation_id`** using
a done-marker file in the request's `out_dir`:

    <out_dir>/.done-<correlation_id>

- Before doing the work, a handler checks whether
  `<out_dir>/.done-<corr>` already exists; if it does, the work counts as
  already done (a re-delivery) and the handler skips re-doing it.
- After finishing (success **or** a terminal error), the handler writes the
  marker, so any later redelivery is a no-op.

`queue.DoneMarker(outDir, corr)` returns that path so every handler computes
it identically. The capworker SDK (`internal/capworker`, the seed of the
full EVB-19 SDK) implements the check-then-write — see
[Cap workers](#cap-workers-and-the-phase-2-gate) below.

## Cap workers and the Phase 2 gate

The **capworker SDK** (`internal/capworker/capworker.go`) is the minimal
consumer a capability worker is built from: it declares its `cap.<cap>`
queue durable with `x-max-priority` 10 and the DLX, consumes with
prefetch 1 and manual ack, and hands each message to a `Handler`. The
reply is published to the request's `reply_to` in **confirm mode before
the ack** — the ack lands only after the broker has confirmed the result
(invariant 3: nothing is lost). A redelivery of an already-done
`correlation_id` (the done-marker exists) skips the work and republishes
the stored result; a request whose deadline passed before pickup is
answered with a `timeout` Result. A dropped connection is re-established
with 1 s → 30 s backoff, so a broker restart mid-message costs at most
one redelivery. `cmd/cap-echo` is the test worker on top of the SDK: the
`cap.echo` capability sleeps `ECHO_DELAY` (30 s on the box) and writes a
small artifact file to the request's `out_dir` — the stand-in for a slow
capability.

The **Phase 2 gate** (`scripts/e2e-cap-echo.sh`, plan task 17) proves the
whole wait/wake loop on the live box with a scripted stand-in round —
`jarvis-jobs mark-running` puts a fresh job running without launching a
round (the local driver is the brain itself, and the plan sanctions the
stand-in), then `jarvis-mq submit` + `listen` + `jarvis-jobs yield`.
Four scenarios:

- **S1** — submit, yield, the echo replies, the switchboard wakes the job
  (the wake lands milliseconds after the artifact, against a 5 s bar).
- **S2** — kill the switchboard mid-wait: the result lands on the durable
  flow queue while it is down, and the restart wakes the job (durable
  result + listener store survive).
- **S3** — restart the broker mid-wait: the durable cap message is
  re-consumed after reconnect and still wakes.
- **S4** — submit to a capability with **no worker** and a short
  `--wait-until`: the switchboard's deadline check wakes the job with
  reason `timeout`.

The gate uses fresh `gate-<epoch>-sN` job ids / flow queues / artifact
dirs under the data root, cleans them up, exits non-zero on any failure,
and restores the switchboard's restart policy on the way out. Its first
run (2026-11-09) caught a real bug: while the broker was down,
`queue.Conn.Reconnect` ran on consecutive ticks — the first attempt's
failed Dial left the connection nil, and the second attempt's
close-of-the-old-connection SIGSEGV'd the switchboard. `Reconnect` and
`Close` now tolerate a nil conn/channel.

## CLI

    jarvis-mq publish flow.avatar --kind agent --title "make avatar" \
        --instructions "generate the image, save it, publish the result to \
        flow.avatar, then add a local agent job that consumes flow.avatar" \
        --context "discord avatar for the identity DM, 512x512" \
        --payload size=512
    jarvis-mq peek flow.avatar            # inspect without removing
    jarvis-mq consume flow.avatar         # pop up to 1 message (or -n N, --json)
    jarvis-mq depth flow.avatar
    jarvis-mq queues                      # all queues with depths
    jarvis-mq purge flow.avatar           # drop the messages, keep the queue
    jarvis-mq delete flow.avatar          # drop the queue too

Capability requests (envelope v2) publish to a `cap.<capability>` queue,
which is created on demand. Any v2 field marks the message v2 (`"v": 2`);
unset fields stay out of the JSON and the AMQP properties:

    jarvis-mq publish cap.image.generate --capability image.generate \
        --correlation abc123 --reply-to flow.job-42 \
        --wake-job job-42 --input-ref ~/.local/share/jarvis/artifacts/job-42/prompt.txt \
        --out-dir ~/.local/share/jarvis/artifacts/job-42/ \
        --location igpu --deadline 2026-11-08T18:00:00Z
    jarvis-mq tail events 'job.#'         # watch lifecycle events on a temp queue

v2 flags: `--capability`, `--correlation`, `--reply-to`, `--wake-job`,
`--parent-job`, `--location`, `--out-dir`, `--input-ref` (repeatable).
`--driver/--class/--effort` map onto the agent-job fields so a published
`kind=agent` message carries its routing.

The capability loop (plan §3.5): park the round on a flow queue, submit
requests to a capability queue, read the results back. Listeners live in
the switchboard's JSON store (one file per listener), so `listen`/`unlisten`
/`listeners` work even while the switchboard service is down:

    jarvis-mq listen flow.job-42 --wake job-42 --timeout 2h   # wake the job
        # or: --forward <queue> (copy to another queue)
        # or: --notify "text"  (Discord message to Nathan)
    jarvis-mq unlisten flow.job-42 [--corr <id>]   # drop its listener(s)
    jarvis-mq listeners                             # table of all listeners

`listen` declares the queue if it does not exist (flow.* / cap.* by name
prefix). `submit` publishes an envelope-v2 request to `cap.<capability>`
— consumer priorities do the placement, there is no router:

    jarvis-mq submit image.generate --reply-to flow.job-42 \
        --corr job-42-img --input prompt="a lighthouse at dusk" \
        --input-ref /tmp/ref.png --out-dir ~/.local/share/jarvis/artifacts/job-42/ \
        --location gpu --vram-mb 12000 --ram-mb 2048 --deadline 2h

`--input k=v` JSON-types the value (512 → number, true → bool, else the
string typed); `--deadline` is RFC3339 or a duration from now. `results`
consumes and pretty-prints `Result` messages from the reply queue (acks
them; `-n N` to cap, `--json` for the raw bodies):

    jarvis-mq results flow.job-42
    jarvis-mq caps    # capability catalog — lands in task EVB-19

Guards: publishing to a queue that is neither `flow.*`, `cap.*` nor a known
queue is refused (the topic exchange would silently drop it), as is `delete`
of a worker-owned queue (voice, fix, coding, background, jobs.*, image) or
of a capability queue (`cap.*`).

## In-process (superagent)

The superagent executor has a `rabbitmq` tool — the same operations as the
CLI (`publish|consume|peek|depth` with the same message fields) — so a
running agent can hand work to another model mid-run without shelling out.

## Code

- `internal/queue/message.go` — the `Message` struct (v1 + v2 envelope fields),
  `Resources`, the `Result` type, the result-status constants, and
  `DoneMarker` (the idempotency marker path).
- `internal/queue/encoding.go` — `encodeMessage`/`DecodeMessage`,
  `EncodeResult`/`DecodeResult`, and `newPublishing` (builds the AMQP
  envelope and mirrors `correlation_id`/`reply_to` onto the properties).
- `internal/queue/flow.go` — `FlowQueueName`, `IsFlowQueue`,
  `EnsureFlowQueue`, `PublishFlow` (confirm-mode, waits for the broker ack),
  `PopFlow`, `PeekFlow`, `PurgeFlow`, `DeleteFlow`.
- `internal/queue/cap.go` — `CapQueueName`, `IsCapQueue`, `EnsureCapQueue`
  (on-demand declare + bind, DLX args — the request side of the v2
  envelope).
- `internal/queue/events.go` — the `events` exchange: `Event`, the
  `JobEvent`/`AgentEvent`/`WindowEvent`/`LeaseEvent` builders, and the
  fire-and-forget `EventPublisher`.
- `internal/queue/dlx.go` — `EnsureDLX`, the `dead` queue, the
  `x-attempts` delivery budget, and `ListDead`/`RequeueDead`/`DropDead`.
- `cmd/jarvis-mq/main.go` — the CLI above.
- `internal/superagent/tools.go` — `runMQ`, the in-process tool.
- `internal/agentjob/continuation.go` — the continuation sidecar:
  `Continuation`, `RenderContinuationMD`/`ParseContinuationMD`, the
  invariant-8 `ContinuationMDFilled` check, `WriteContinuation`/
  `ReadContinuation`, `RecordedResults`/`MissingResults`, and the shared
  `WaitAge`/`WaitRemaining` renderers.
- `internal/agentjob/wait.go` — the waiting state machine (EVB-12):
  `Store.Yield`, `Store.Wake`, `Store.RecordResult`.
- `cmd/jarvis-jobs/yield.go` — `jarvis-jobs yield`, `waiting`, `wake`,
  and `mark-running` (the stand-in-round primitive the Phase 2 gate uses).
- `internal/capworker/capworker.go` — the capworker SDK: queue ensure,
  prefetch-1 consume, the done-marker idempotency, the deadline-before-
  pickup `timeout`, and the confirm-mode result-before-ack.
- `cmd/cap-echo/main.go` — the `cap.echo` test worker on top of the SDK.
- `scripts/e2e-cap-echo.sh` — the Phase 2 wait/wake gate (S1–S4).
