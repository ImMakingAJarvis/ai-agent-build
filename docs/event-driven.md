# Event-driven work: flow queues

The worker queues (voice/fix/coding/background, jobs.gpu/cpu/igpu) move
*jobs* from the scheduler to a model. **Flow queues** (`flow.<name>`) are the
event-driven channel between models *across rounds*: one round publishes a
job to a flow queue, finishes; another model does the work and publishes the
result back; the first model's next round consumes it and continues.

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
it identically. The capworker SDK (EVB-19) implements the check-then-write;
this task fixes only the convention and the path helper.

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

Guards: publishing to a queue that is neither `flow.*` nor a known queue is
refused (the topic exchange would silently drop it), as is `delete` of a
worker-owned queue (voice, fix, coding, background, jobs.*, image).
`--driver/--class/--effort` map onto the agent-job fields so a published
`kind=agent` message carries its routing.

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
- `cmd/jarvis-mq/main.go` — the CLI above.
- `internal/superagent/tools.go` — `runMQ`, the in-process tool.
