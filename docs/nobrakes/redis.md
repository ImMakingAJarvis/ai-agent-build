# NoBrakes — Redis flag store

NoBrakes (autonomous job execution — the nightly batch and full auto mode)
keeps all of its shared state in
the stack's existing Redis service. Nothing new runs: the `redis` service in
`docker-compose.yml` is the one used for live job streams, and NoBrakes
namespaces its keys under `nobrakes:`.

## What runs where

- **Service**: `redis` in `docker-compose.yml` (repo root), container
  `jarvis-redis`, image `redis:7`.
- **Port**: `6379`, bound to `127.0.0.1` only — no LAN exposure, no
  password (trust boundary is the loopback bind).
- **Boot**: started by the `jarvis-stack` systemd unit (enabled) as part of
  `docker compose up -d`; container policy `restart: unless-stopped`.
- **State**: named volume `redis_data`, append-only file on
  (`--appendonly yes`), `maxmemory 256mb` with `noeviction` — keys are never
  evicted, so a full Redis means a loud error, not a silently lost flag.
- **Access**:
  - host-side Go code: `redis://127.0.0.1:6379` (the default in
    `internal/agentjob.StreamAddr` / `$JARVIS_REDIS`)
  - in-stack containers: `redis://redis:6379`
  - shell: `docker exec jarvis-redis redis-cli ...`

## Configuration

There is no `redis.conf` file: the flags live in the `command:` line of the
`redis` service in `docker-compose.yml`. No NoBrakes-specific config, no
secrets. To change memory/eviction, edit that line and
`docker compose up -d redis` (or `restart redis`).

## Key structure

| Key | Type | Value | Default |
|---|---|---|---|
| `nobrakes:auto_mode` | string | `"on"` or `"off"` | `"off"` (also the read result for a missing or corrupt value — the feature fails safe) |
| `nobrakes:nightly_flag` | string | `""` or a UTC date `"YYYY-MM-DD"` | `""` |
| `nobrakes:full_auto` | string | `"on"` or `"off"` | `"off"` (also the read result for a missing or corrupt value — the feature fails safe) |
| `nobrakes:running` | string | the id of the one auto job currently executing | absent (no auto job running) |

- **`nobrakes:auto_mode`** — the operator's master switch for autonomous
  nightly execution. Written by the manual-override path (the Discord
  `!automode on/off` daemon, nobrakes-3) and read by the nightly gate
  (nobrakes-2, `nobrakes.AutoEnqueue` — see below). Must be `"on"` for any
  auto-enqueue to happen.
- **`nobrakes:nightly_flag`** — idempotency marker for the nightly window.
  - `""` = the nightly enqueue has never run.
  - `"YYYY-MM-DD"` = the UTC date of the last nightly window whose jobs were
    already enqueued.
  The nightly trigger fires only when `auto_mode == "on"` **and** the flag is
  not today's UTC date; after the enqueue it sets the flag to today's date.
  Worker restarts or repeated triggers on the same day therefore cannot
  double-enqueue the nightly batch, and the window re-arms automatically the
  next UTC day. No TTL — it is compared across days.
- **`nobrakes:full_auto`** — the full-auto switch. When `"on"`,
  the agent-run loop runs the next ready job **immediately, at any time of
  day**, for as long as the queue has work, without asking the owner who
  runs it; with an empty queue it idle-polls instead of asking. It is
  independent of `auto_mode` (the nightly batch) — the two can be combined.
  Written by the operator (`jarvis-jobs fullauto on|off`, Discord
  `!fullauto`), read by the agent-run loop at the top of every stage-3
  iteration. Missing or corrupt values read as `"off"` — the supervised
  ask path is the default.
- **`nobrakes:running`** — the safety guardrail (nobrakes-6): marks the
  single auto job that is executing right now. Value: the job id. It is
  created by `jarvis-jobs run -auto` (the full-auto path of the agent-run
  loop) and released when the round ends, whatever the outcome; it has no
  TTL, because the guarantee it protects (at most one concurrent auto
  job) must not depend on a clock. A marker left behind by a crash is
  *stale* and is taken over automatically: the next `run -auto` checks
  the holder's job status in the job store and overwrites the marker when
  the holder is no longer `running`. An operator can inspect it with
  `jarvis-jobs running` and clear a stuck one with
  `jarvis-jobs running clear`. The three flag keys are pre-created at
  install time (`EnsureDefaults` in `internal/nobrakes` does the same
  idempotently at runtime — it only fills missing keys and never clobbers
  operator state); `nobrakes:running` is transient and only exists while
  an auto job runs.

## Go helper

`internal/nobrakes` (import as `nobrakes`):

```go
st, err := nobrakes.New("redis://127.0.0.1:6379")  // or $JARVIS_REDIS
on, _ := st.AutoModeOn(ctx)          // false when key missing/corrupt
_ = st.SetAutoMode(ctx, true)        // manual override
due, _ := st.NightlyDue(ctx, time.Now())  // idempotency check
_ = st.SetNightlyFlag(ctx, time.Now().UTC().Format("2006-01-02"))
fa, _ := st.FullAutoOn(ctx)          // false when key missing/corrupt
_ = st.SetFullAuto(ctx, true)        // manual override (full auto mode)
```

Constants: `nobrakes.AutoModeKey`, `nobrakes.NightlyFlagKey`,
`nobrakes.FullAutoKey`, `nobrakes.AutoModeOn/Off`. Unit-tested against
miniredis in `internal/nobrakes/nobrakes_test.go`; later NoBrakes tasks
reuse this store.

## The nightly gate (nobrakes-2)

`nobrakes.AutoEnqueue` is the nightly gate: when `auto_mode` is `"on"` and
today's UTC window is unprocessed, it moves every parked nightly job to
`ready` and stamps the window. It is driven by the agent-run loop, which
calls `jarvis-jobs nightly` at the top of every stage-3 iteration:

```bash
jarvis-jobs nightly          # the gate, one pass (called by agent-run)
jarvis-jobs list parked      # what is waiting for the next window
jarvis-jobs enqueue <id>     # run one parked job now, without waiting
```

Behaviour, in order:

1. **`auto_mode` off or missing** → nothing happens (the default; no
   auto-enqueue, no key writes).
2. **on, window unprocessed** → every parked nightly-tagged job is
   enqueued (no manual trigger) and `nightly_flag` is stamped with today's
   UTC date. The stamp happens *after* the enqueues, so a failed pass
   retries the same day without double-enqueueing; with zero parked jobs it
   still stamps — a nightly job added after the check waits for the next
   window, which is what makes it nightly.
3. **on, window already stamped today** → no-op (idempotent across loop
   iterations and restarts).
4. **Redis unreachable or disabled** (`$JARVIS_REDIS=off`) → one warning
   line and exit 0; the feature fails safe to off and never stalls or kills
   the loop.

A nightly job is stored **parked** — present in the store but out of the
active queue (`next` and the dashboard queue view never see it). The
designation itself lives on the kanban board (nobrakes-4, below); the
job-level tag that parks it at ingest is the storage mechanism.

## The nightly tag (nobrakes-4)

The kanban task's `nightly` flag is the canonical source of the NoBrakes
designation: a task is "nightly" when `Task.Nightly` is set, and the set of
auto-eligible jobs is derived from it.

- **Board** — the flag is stored on the task JSON
  (`internal/kanban`: `Task.Nightly`, `UpdateTaskNightly`,
  `ListNightlyTasks`) and is set/cleared from the board UI (the task
  detail's Nightly checkbox) or the API (`PATCH /api/kanban/tasks/{id}`
  with `{"nightly": true|false}`; `GET /api/kanban/tasks?nightly=true`
  lists the nightly set; create accepts `"nightly": true`).
- **Link** — a job points at its task with
  `jarvis-jobs add ... -kanban-task <task-id>` (`Spec.KanbanTaskID`).
  When the linked task is nightly, add also applies the job-level
  `nightly` tag, so `Ingest` stores the job parked.
- **Gate** — `nobrakes.NightlyEligible` walks the parked jobs: a job
  whose `KanbanTaskID` resolves on the board is eligible exactly when the
  task is `Nightly` (the board wins, even over a stale job tag); a job
  whose link is missing or unresolvable falls back to the job-level tag,
  so jobs queued before nobrakes-4 keep working. `AutoEnqueue` uses this
  set; `jarvis-jobs nightly` opens the board and passes it in, falling
  back to job tags with a warning if the board cannot be read.

So the loop is: tag a kanban task nightly on the board → jobs created
from it (`-kanban-task`) park → with `!automode on`, the first
`jarvis-jobs nightly` pass of each UTC day enqueues exactly those jobs.

## The audit log (nobrakes-5)

Every job the gate auto-enqueues is recorded in a durable log, so
autonomous execution is reviewable after the fact: what ran, when, and
that it was automatic.

- **Location & format** — `<data dir>/nobrakes/audit.jsonl` (default
  `~/.local/share/jarvis/nobrakes/audit.jsonl`), one JSON object per line:
  `{"timestamp":"…","window":"YYYY-MM-DD","job_id":"…","title":"…",
  "trigger":"auto_mode"}`. `timestamp` is the pass time (UTC, RFC 3339);
  `window` is the nightly window the pass processed, so entries group per
  batch. `trigger` is always `auto_mode` today; the field exists so a
  future trigger (scheduled run, operator command) is distinguishable in
  the same log.
- **Written** — by `AutoEnqueue`, appended immediately after each
  `Enqueue` succeeds, carrying that pass's `now` (deterministic for tests).
  A failed audit write never blocks the batch: the job runs either way and
  the shortfall is reported. If the log file itself cannot be opened, the
  pass runs unaudited (the enqueue line is still in the loop log).
- **Read** — `jarvis-jobs audit [n]` (last `n` entries, default all;
  `-json` for the raw lines) and the dashboard's
  `GET /api/nobrakes/audit` (every entry, oldest first, plus the log path).
  A missing file reads as an empty list, not an error.
- **Robustness** — each entry is a single O_APPEND write, so a crash cannot
  corrupt earlier lines; a torn final line (crash mid-append) is skipped on
  read, so one bad line cannot hide the rest.

Manual enqueues (`jarvis-jobs enqueue`) are not auto-executions and are not
audited; the log records the gate's own actions only.

## Full auto mode

When `full_auto` is `"on"`, the agent-run stage-3 loop runs the next ready
job **immediately, at any time of day** — not just the nightly window — for
as long as the queue has work. Each iteration:

1. **flag read**: `jarvis-jobs fullauto` reads `nobrakes:full_auto` fresh
   (the read fails safe to `"off"` — Redis disabled or unreachable, corrupt
   value, or a read error — so the loop can never stall or run blind).
2. **on + a ready job** → the job runs right away, without a Discord
   question. The driver is the job's own spec driver (`jarvis-jobs show`
   reports it as `driver:` — e.g. a sonnet-gated job runs on Sonnet),
   falling back to the driver of the last round, then `local`. A short
   Discord notify announces what is running.
3. **on + empty queue** → the loop idle-polls (60 s) instead of asking;
   newly queued jobs start on the next pass.
4. **off** → the supervised path is unchanged: the loop asks on Discord
   which driver runs the job (or to skip/stop).

The two switches are independent: `auto_mode` still controls the nightly
auto-enqueue (section above), `full_auto` controls per-job execution. The
owner overrides at any time — `!fullauto off` takes effect on the next loop
iteration, and `!pause` / `!restart` still interrupt a running job.

## The control surface (nobrakes-3, extended in nobrakes-7)

The owner flips `auto_mode` from Discord with the `!automode` command in the
`jarvis-chat` daemon, and `full_auto` with `!fullauto`. Both
are owner-only by construction — the chat layer only fires the handler for
the owner DM, the same guarantee as `!pause`/`!restart`:

    !automode on     # auto_mode -> "on": nightly jobs will auto-enqueue
    !automode off    # auto_mode -> "off": nightly jobs stay parked
    !fullauto on     # full_auto -> "on": ready jobs run immediately, any time
    !fullauto off    # full_auto -> "off": back to the supervised ask

The command dials the flag store (`$JARVIS_REDIS`, default
`redis://127.0.0.1:6379`), writes the value, reads it back, and confirms the
stored result — turning it on warns that jobs will now run autonomously. A
bad or missing argument gets a usage line and leaves the flag untouched;
Redis unreachable or disabled leaves the flag unchanged and says so.
`cmd/jarvis-chat/automode.go` and `cmd/jarvis-chat/fullauto.go`
(unit-tested against miniredis).

The brain can flip the same switches in natural language ("turn on full
auto"). Hermes registers `set_automode` / `set_fullauto` actions
(`internal/hermes/automode.go`), owner-only by full access; they dial the
same flag store, set the same keys, and confirm with the same shared
reply text (`nobrakes.AutoModeReply` / `FullAutoReply`). The classifier is
told to use these instead of `enqueue_job` — a mode change is a switch,
never a job — and to classify messages that need no action as chat, so
doing nothing is a first-class outcome.

### Waking the loop (nobrakes-7)

A flag only means something while the agent-run loop runs, so the switch
commands also look at the loop:

- **`!fullauto on` / `set_fullauto on`** — after the flag is stored,
  `nobrakes.WakeLoop` (`internal/nobrakes/wake.go`) checks whether the
  `agent-run` unit is active; when it is stopped it runs the
  `agent-run-restart` helper, which starts the service (and is a no-op
  while the loop is inhibited by `agent-run.disabled` or `resume-pending`).
  The confirmation gains a note: "…I started it", or what is standing in
  the way when it could not. A wake failure never blocks or rolls back the
  flag write — the note just says the switch takes effect once the loop is
  up.
- **`!automode on` / `set_automode on`** — when the loop is stopped, the
  confirmation says so ("…nothing will run until it is started") but does
  **not** start it: a started-but-supervised loop would ask on Discord who
  runs the next job, which the owner did not ask for.

## Health & operations

```bash
docker exec jarvis-redis redis-cli ping                     # PONG
docker exec jarvis-redis redis-cli GET nobrakes:auto_mode   # off
docker exec jarvis-redis redis-cli EXISTS nobrakes:auto_mode nobrakes:nightly_flag nobrakes:full_auto  # 3
docker exec jarvis-redis redis-cli GET nobrakes:running     # (nil) while no auto job runs

# restart (container comes back with state intact — AOF)
docker compose restart redis

# stop just redis
docker compose stop redis

# stop the whole stack (redis comes back with it)
sudo systemctl stop jarvis-stack
```

If the feature must be killed instantly while Redis keeps serving job
streams: `docker exec jarvis-redis redis-cli SET nobrakes:auto_mode off` —
the nightly gate reads this on every run. For full auto mode:
`docker exec jarvis-redis redis-cli SET nobrakes:full_auto off` — the loop
re-reads it at the top of every iteration and returns to the supervised
ask. A round that is *running* can be interrupted the usual way
(`!pause` / `!restart`); if the `nobrakes:running` marker is ever left
behind and the auto path stops picking up jobs, check `jarvis-jobs
running` and clear it with `jarvis-jobs running clear` (a stale marker is
also taken over automatically once the holder's job is no longer running).
The full safety model is in [safety.md](safety.md).
