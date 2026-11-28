---
id: loopd
name: Loopd (event-loop runtime)
summary: The event-loop agent runtime daemon — serves the runtime API and runs the scheduler loop for jobs with driver loop; drains on SIGTERM.
group: agent
kind: container
status: active
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
`cmd/loopctl`); since ELR-100 (M4.1h) it is a normal compose service —
it starts with the stack, published on 127.0.0.1:9110 only. Before every
op the loop consults the permission layer (card ELR-107, M4.3b):
`perm.Decide` allows the op, refuses it (the op fails at once, its tool
result `denied: <the matching rule>`), or sends it to the owner (the op
is parked awaiting_approval with a pending approval, and the job waits
on the approval holding no slot — the tick releases the slot before the
op starts). A recorded decision on a parked approval acts on it (card
ELR-108, M4.3c): approved starts the op through the run path that
skips the gate; denied finishes it as a failed op whose tool result is
`denied: <the note>`, and the job goes back to ready either way it can
wake; a decision for a job cancelled meanwhile changes nothing, and
cancelling a job parked on an approval (store stop) closes its parked
ops as cancelled so nothing waits on a decision that will never come.

## Interfaces

- `GET /healthz` on `loopd.listen` (default `:9110`; the `LOOPD_ADDR`
  env, when set, still overrides it) — returns 200 and the text `ok`.
  Containers on the compose network reach it at
  `http://loopd:9110/healthz`.
- `loopctl drain [--addr URL] [--token-file PATH] [--wait DURATION]`
  (ELR-99, M4.1g) — asks the daemon to stop taking new work: POST
  `/v1/drain` (the bearer token read from `--token-file`, default
  `/run/loopd/token`, against `--addr`, default `http://127.0.0.1:9110`),
  prints the running ops one `job/op` pair per line, and with `--wait`
  keeps polling `GET /v1/state` until `running_ops` is empty (exit 0)
  or the duration is up (exit 1, the still-running ops printed).
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
  - `POST /v1/tasks` creates a task through `Deps.Submit`.
    `cmd/loopd` wires `api.NewSubmit` (ELR-104, M4.2d): it generates the
    branch `loop/<id>` when the request names none, refuses a branch a
    live job already uses — any status except done, failed, skipped and
    parked (the plan's "stopped") — and a branch checked out in another
    worktree (`workspace.ErrBranchInUse`), both as an error wrapping
    `api.ErrConflict`; it then `Create()`s the task's worktree (cut from
    `HEAD` of the repo at `LOOPD_REPO`), sets `Spec.Workspace` to the
    worktree's path, `Ingest`s the job (id `task-<unix seconds>-<8 hex>`
    from crypto/rand, class `build`, source `api`), and hands the id to
    the loop's `Submit` — any error after the worktree exists removes it
    again. 201 `{"id":..}`; 400 on bad JSON or empty instructions; 409
    on a branch conflict. A server without a `Deps.Submit` keeps the
    default: `Ingest` + `Deps.Submitter`, no worktree.
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
- `LOOPD_REPO` — the git repo the task worktrees are cut from (ELR-104,
  M4.2d); unset, `NewSubmit`'s `ws.Create` fails and `POST /v1/tasks`
  answers 500.
- The runtime config file (`$LOOPD_CONFIG`, default
  `~/.config/jarvis/loopd.yaml`) carries the `loopd` keys (ELR-93,
  M4.1a), defined and validated in `runtime/config`:
  - `loopd.token_file` — bearer token file, default `/run/loopd/token`
    (must be owned and private, no group/other bits).
  - `loopd.shutdown_grace_seconds` — default 60, must not be negative.
  - `loopd.listen` — default `:9110`, must not be empty.
  - `workspace.root` — the task-worktree root, default
    `/var/lib/jarvis/workspaces`, must not be empty (card ELR-105,
    M4.2e); the container's worktree mount is this directory.
  - `workspace.repo` — the git repo the task worktrees are cut from,
    default `/home/agent/ai-agent-build`, must not be empty (card
    ELR-105); the daemon still reads the `LOOPD_REPO` env for the repo
    path (card ELR-104).
  - `approval_expiry_hours` — how long a parked approval waits before
    it expires; default 24, must not be negative (card ELR-107, M4.3b).
  - `approval_remind_minutes` — how long after the request the owner is
    reminded of a pending approval; default 60, must not be negative.
  - `owner_discord_id` — the owner's Discord id approval requests and
    reminders are addressed to; default empty.
- No compose profile since ELR-100 (M4.1h): it starts with plain
  `docker compose up -d`.
- `read_only: true` and `tmpfs: /tmp` (card ELR-105, M4.2e): the
  rootfs is read-only; the container writes only to the job store, the
  workspace root and the slot dir (the Data mounts) and to /tmp.
  `GOCACHE` and `GOMODCACHE` are pinned to /tmp so the in-container Go
  builds and tests keep working on the read-only rootfs.
- The bearer token file `~/.local/share/jarvis/secrets/loopd-token`
  (mode 0600) is bind-mounted read-only at `/run/loopd/token` — the
  default of `loopd.token_file`.
- `stop_grace_period: 70s` — `loopd.shutdown_grace_seconds` (default 60)
  plus 10s of headroom; raise it in compose if the grace is raised.
- `env_file: agent.env`, like the other agent services.
- `HOME: /home/agent` — the image has no passwd entry for uid 1001, so
  the runtime leaves HOME at "/" and `agentjob.DataDir()` (which reads
  $HOME) would resolve the job store to `/.local/share/jarvis` (card
  ELR-100: the container crash-looped on the lock's mkdir until HOME
  was pinned at the mount point).

## Data

- The job store (`~/.local/share/jarvis`), mounted into the container:
  the daemon reads and writes the job specs, states, ops, and message
  logs (ELR-8, ELR-9) through `agentjob.Open(agentjob.DataDir())`.
- `loop.lock` in the store directory — the single-runtime flock
  (ELR-87) the daemon holds for its whole life; the kernel releases it
  when the process dies.
- Workspace root `/var/lib/jarvis/workspaces` (`workspace.root`,
  default): one of the three writable mounts (card ELR-105, M4.2e) —
  the bind mount that holds the task worktrees.
- Engine slot dir `/var/lib/jarvis/slots` (`engine.slot_dir`,
  default): the writable mount for slot save and restore (card
  ELR-105).
- Task worktrees (ELR-104, M4.2d) under `workspaces/<jobID>` in the
  store directory — one per submitted task, cut from `HEAD` of
  `LOOPD_REPO`, created by `POST /v1/tasks` and removed again when the
  submit fails after the create.
- Approval requests the loop parks for the owner live under the job's
  `approvals/` directory in the job store — one JSON per request,
  pending until decided or expired (card ELR-107); a recorded decision
  is acted on by the loop (card ELR-108).
- Restart survival (decision, card ELR-100, M4.1h): operations do NOT
  survive a container restart — the children die with the container.
  loopd relies on Recover (M1.2e) plus resume (card M8.5) to adopt or
  finish them at the next start, and the deploy guard (card ELR-99)
  refuses a restart while ops run. There is no separate loop-ops
  container in M4.
- No other state: the loop's metrics registry is in-process and resets
  on restart.

## Deploy

    scripts/install_loopd.sh [--force]

The script builds the image and runs
`docker compose --profile loop build loopd && docker compose
--profile loop up -d loopd`, then waits for `/healthz` — the
`--profile loop` flags are inert since the service lost its profile
(ELR-100); the plain equivalent is `docker compose build loopd &&
docker compose up -d loopd`. The deploy
guard (ELR-99, M4.1g) runs first: it asks the running daemon
`GET /v1/state` (token from `$LOOPD_TOKEN_FILE`, default
`/run/loopd/token`; address `$LOOPD_API_URL`, default
`http://127.0.0.1:9110`) and exits 1 with the running ops when
`running_ops` is not empty, unless `--force` is given (which names the
ops and goes ahead). When the daemon is not reachable — or the token
file is missing, so it cannot be up — the install proceeds.
Before the first `up`, the writable mount sources must exist on the
host owned by the agent user (`/var/lib/jarvis/workspaces`,
`/var/lib/jarvis/slots`): Docker creates a missing bind source
root-owned, which the container's uid 1001 cannot write.

## Verify live

    docker compose build loopd && docker compose up -d loopd
    curl http://127.0.0.1:9110/healthz          # → ok
    curl http://127.0.0.1:9110/metrics          # → 200, the loop's series
    curl http://127.0.0.1:9110/v1/state         # → 401 {"error":"unauthorized"}
    curl -H "Authorization: Bearer $(cat /run/loopd/token)" \
         http://127.0.0.1:9110/v1/state         # → 200 {"draining":false,...}
    docker cp scripts/loopd-image-check.sh jarvis-loopd:/tmp/
    docker exec jarvis-loopd bash /tmp/loopd-image-check.sh
                                              # one line per capability: ok/MISSING
    docker exec jarvis-loopd sh -c 'touch /etc/x'
                                              # must FAIL: read-only rootfs
    docker exec jarvis-loopd sh -c \
      'touch /var/lib/jarvis/workspaces/x && rm /var/lib/jarvis/workspaces/x'
                                              # must work: writable worktree mount
    docker logs --tail 20 jarvis-loopd          # loopd listening on :9110

The API is loopback-only (published on 127.0.0.1:9110): the verify
commands run on the box itself; from other machines the port is not
reachable. The image-check script is not in the image (the report on
card ELR-100), hence the docker cp.

## Failure modes

- The deploy guard (ELR-99): `scripts/install_loopd.sh` exits 1 with
  the running ops while `running_ops` is not empty; that is the guard
  working — drain first (`loopctl drain --wait 10m`) or pass `--force`.
- An unusable token file (missing, empty, or group/other readable —
  `mode & 0o077` != 0, ELR-93) makes the daemon fail to start: `run`
  returns before the listener opens (card ELR-98).
- Another runtime holds `loop.lock` (ELR-87): the daemon prints the
  holder and exits 3 without serving anything.
- SIGTERM with a tick or ops still running at the grace deadline: the
  daemon exits 0 anyway and names what is still running in the log line
  (`shutdown deadline reached; still running: ...`); the ops are adopted
  or reported lost on the next start (ELR-90).
- A hard restart (crash, or a forced redeploy): the running ops die
  with the container — there is no loop-ops container (decision, card
  ELR-100); Recover plus resume (card M8.5) adopts or reports them
  lost at the next start, and the deploy guard (ELR-99) refuses a
  graceful restart while ops run.
