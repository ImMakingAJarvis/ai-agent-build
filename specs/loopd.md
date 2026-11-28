---
id: loopd
name: Loopd (event-loop runtime)
summary: The event-loop agent runtime daemon — serves the runtime API and runs the scheduler loop for jobs with driver loop; drains on SIGTERM.
group: agent
kind: container
status: optional
compose: [loopd]
binaries: [cmd/loopd, cmd/loopctl]
ports: ["9110 API (loopd.listen, default :9110; the LOOPD_ADDR env still overrides it; published on 127.0.0.1 only)"]
health: http://loopd:9110/healthz
---

## Purpose

The event-loop agent runtime daemon. It reads the bearer token (failing
to start when the file is unusable), serves the runtime API behind the
bearer-token guard, and runs the scheduler: one tick at a time for jobs
with driver loop, sleeping the idle the loop measures when Step reports
no work, and no new tick once the drain has begun. On SIGTERM it starts
the drain, waits up to `loopd.shutdown_grace_seconds` for the running
tick and its ops, shuts the HTTP server down, and exits 0 (card ELR-98,
M4.1f). Card ELR-6 (M1.1a) created the homes (`runtime/`, `cmd/loopd`,
`cmd/loopctl`); since ELR-7 it builds as a compose service behind the
`loop` profile.

## Interfaces

- `GET /healthz` on `loopd.listen` (default `:9110`; the `LOOPD_ADDR`
  env, when set, still overrides it) — returns 200 and the text `ok`.
  Containers on the compose network reach it at
  `http://loopd:9110/healthz`.
- `loopctl version` — prints `loopctl dev`; any other argument prints
  `usage: loopctl version` to stderr and exits 1.
- Bearer-token guard (ELR-93, M4.1a): `runtime/api.Guard` requires the
  header `Authorization: Bearer <token>` on every path and method — GET
  included, nothing is served without it — and answers 401
  `{"error":"unauthorized"}` otherwise; `/healthz` and `/metrics` are
  exempt. `runtime/api.LoadToken` reads the token from the file at
  `loopd.token_file` and rejects a missing file, an empty file, and a
  mode with any group/other bit set (`mode & 0o077`). The daemon reads
  it at start, fails to start when it is unusable, and serves the API
  behind the guard (card ELR-98, M4.1f).
- Read API (ELR-94, M4.1b): `runtime/api.Server` (`New(Deps{Store,
  Runner, Redactor, Token, Version})`, `Handler()`) serves, behind the
  same Guard, `GET /v1/tasks/{id}` (the job's spec and state plus its
  ops; 404 `{"error":"not found"}` for an unknown id),
  `GET /v1/tasks/{id}/messages` (the job's conversation log as a JSON
  array, empty array for a job with no log), and `GET /v1/state`
  (`{"draining":..,"running_ops":[{"job":..,"op":..}],"jobs":{
  status:count}}` — `draining` reads the Drainer (M4.1e; false when
  the Server has none);
  `running_ops` joins the Runner's live ops to the store's running
  ops). Every string in a served message or log passes the Redactor
  before it is written (edge case 2026-11-24): no endpoint returns raw
  text. All the read routes are mounted in `cmd/loopd` (M4.1f).
- Write API (ELR-95, M4.1c), behind the same Guard:
  - `POST /v1/tasks` creates a task through `Deps.Submit` (default:
    `Ingest`ed into the store as a ready loop job — id
    `task-<unix seconds>-<8 hex>` from crypto/rand — then handed to
    `Deps.Submitter`, the loop's `Submit`, wired by `cmd/loopd`; card
    m4-2d wraps the default with the submit checks). 201 `{"id":..}`;
    400 on bad JSON or empty instructions; 409 when Submit returns an
    error wrapping `api.ErrConflict`.
  - `POST /v1/tasks/{id}/cancel` cancels the job's running ops through
    `Runner.Cancel` and sets the job status to stopped — the store's
    `parked` status (the plan's "stopped/parked"): out of the active
    queue, its ready dependents dep-blocked by the sweep. 200
    `{"id":..,"status":..}`; 404 for an unknown id; a second cancel, or a
    cancel of a finished job (done/failed/skipped), is a 200 no-op.
  - `POST /v1/tasks/{id}/messages` appends `{"text":..}` to the job's
    conversation log (messages.jsonl) as a user message; 200 with the
    message's seq, 400 on bad JSON or empty text, 404 for an unknown job.
  - `POST /v1/tasks/{id}/approvals` is registered and answers 501
    `{"error":"not implemented"}` until card m4-3d replaces it.
  Config changes are not an endpoint in M4.
- Drain (ELR-97, M4.1e), behind the same Guard:
  `POST /v1/drain` starts the drain through `Deps.Drainer.Begin` and
  answers at once (it does not wait) with
  `{"draining":true,"running_ops":[...]}` — the same `job`/`op` join as
  `GET /v1/state`. The drain state is `runtime/api.Drainer`
  (`NewDrainer`, idempotent `Begin`, `Draining`, and `Wait(ctx,
  running)` which polls every 200 ms until `running()` is empty or the
  context ends and returns the op ids still running). Scheduler
  contract card m4-1f builds on: once `Draining()` is true no new tick
  is started; a tick already running finishes and checkpoints. A
  nil `Deps.Drainer` answers 503 and reports not draining.
- Event stream (ELR-96, M4.1d), behind the same Guard:
  `GET /v1/events` streams the runtime's events as server-sent events —
  `id: <id>\nevent: <type>\ndata: <json>\n\n`, flushed after each event.
  The stream ends when the client goes away or the broker closes. The
  broker is `runtime/api.Broker`: `NewBroker(buffer)` gives each
  subscriber a buffered channel, `Publish` assigns the increasing id and
  never blocks (a subscriber whose channel is full drops the event), and
  `Subscribe` returns the channel and a cancel that closes it. Every
  event's data passes the Redactor (re-marshalled string fields) before
  it is written. The daemon wires the runtime's events into `Publish`
  (M4.1f): `op.progress` from `Runner.OnProgress`, `op.done` from
  `Runner.OnDone` (data: the result's status), and one `tick` event at
  the end of every tick that ran.

## Configuration

- `LOOPD_ADDR` — the old env override (ELR-6): when set, it wins over
  `loopd.listen`.
- The runtime config file (`$LOOPD_CONFIG`, default
  `~/.config/jarvis/loopd.yaml`) carries the `loopd` keys (ELR-93,
  M4.1a), defined and validated in `runtime/config`:
  - `loopd.token_file` — bearer token file, default `/run/loopd/token`
    (must be owned and private, no group/other bits).
  - `loopd.shutdown_grace_seconds` — default 60, must not be negative.
  - `loopd.listen` — default `:9110`, must not be empty.
- Compose profile `loop` — it never starts with `docker compose up -d`.
- `env_file: agent.env`, like the other agent services.

## Data

- The job store (`~/.local/share/jarvis`), mounted into the container:
  the daemon reads and writes the job specs, states, ops, and message
  logs (ELR-8, ELR-9) through `agentjob.Open(agentjob.DataDir())`.
- `loop.lock` in the store directory — the single-runtime flock
  (ELR-87) the daemon holds for its whole life; the kernel releases it
  when the process dies.
- No other state: the loop's metrics registry is in-process and resets
  on restart.

## Deploy

    docker compose --profile loop build loopd && docker compose --profile loop up -d loopd

## Verify live

    docker compose --profile loop up -d loopd
    curl http://127.0.0.1:9110/healthz          # → ok
    curl http://127.0.0.1:9110/metrics          # → 200, the loop's series
    curl http://127.0.0.1:9110/v1/state         # → 401 {"error":"unauthorized"}
    curl -H "Authorization: Bearer $(cat /run/loopd/token)" \
         http://127.0.0.1:9110/v1/state         # → 200 {"draining":false,...}
    docker logs --tail 20 jarvis-loopd          # loopd listening on :9110

Outside the profile it is expected to be absent (the map shows it down).

## Failure modes

- An unusable token file (missing, empty, or group/other readable —
  `mode & 0o077` != 0, ELR-93) makes the daemon fail to start: `run`
  returns before the listener opens (card ELR-98).
- Another runtime holds `loop.lock` (ELR-87): the daemon prints the
  holder and exits 3 without serving anything.
- SIGTERM with a tick or ops still running at the grace deadline: the
  daemon exits 0 anyway and names what is still running in the log line
  (`shutdown deadline reached; still running: ...`); the ops are adopted
  or reported lost on the next start (ELR-90).
