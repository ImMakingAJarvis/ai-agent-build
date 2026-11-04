# NoBrakes — Redis flag store

NoBrakes (autonomous nightly job execution) keeps all of its shared state in
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

Both keys are pre-created at install time (`EnsureDefaults` in
`internal/nobrakes` does the same idempotently at runtime — it only fills
missing keys and never clobbers operator state).

## Go helper

`internal/nobrakes` (import as `nobrakes`):

```go
st, err := nobrakes.New("redis://127.0.0.1:6379")  // or $JARVIS_REDIS
on, _ := st.AutoModeOn(ctx)          // false when key missing/corrupt
_ = st.SetAutoMode(ctx, true)        // manual override
due, _ := st.NightlyDue(ctx, time.Now())  // idempotency check
_ = st.SetNightlyFlag(ctx, time.Now().UTC().Format("2006-01-02"))
```

Constants: `nobrakes.AutoModeKey`, `nobrakes.NightlyFlagKey`,
`nobrakes.AutoModeOn/Off`. Unit-tested against miniredis in
`internal/nobrakes/nobrakes_test.go`; later NoBrakes tasks reuse this store.

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

## The control surface (nobrakes-3)

The owner flips `auto_mode` from Discord with the `!automode` command in the
`jarvis-chat` daemon. It is owner-only by construction — the chat layer only
fires the handler for the owner DM, the same guarantee as `!pause`/`!restart`:

    !automode on     # auto_mode -> "on": nightly jobs will auto-enqueue
    !automode off    # auto_mode -> "off": nightly jobs stay parked

The command dials the flag store (`$JARVIS_REDIS`, default
`redis://127.0.0.1:6379`), writes the value, reads it back, and confirms the
stored result — turning it on warns that nightly jobs will now run
autonomously. A bad or missing argument gets a usage line and leaves the flag
untouched; Redis unreachable or disabled leaves the flag unchanged and says
so. `cmd/jarvis-chat/automode.go` (unit-tested against miniredis).

## Health & operations

```bash
docker exec jarvis-redis redis-cli ping                     # PONG
docker exec jarvis-redis redis-cli GET nobrakes:auto_mode   # off
docker exec jarvis-redis redis-cli EXISTS nobrakes:auto_mode nobrakes:nightly_flag  # 2

# restart (container comes back with state intact — AOF)
docker compose restart redis

# stop just redis
docker compose stop redis

# stop the whole stack (redis comes back with it)
sudo systemctl stop jarvis-stack
```

If the feature must be killed instantly while Redis keeps serving job
streams: `docker exec jarvis-redis redis-cli SET nobrakes:auto_mode off` —
the nightly gate reads this on every run.
