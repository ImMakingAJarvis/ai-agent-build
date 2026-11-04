# NoBrakes — safety guardrails (6/6)

NoBrakes lets the machine run its own job queue without asking. This is
the safety layer that makes that acceptable to leave on: **at most one
auto job at a time, an auto failure stops the feature and tells the
owner, and a human can always drive the queue by hand.** It is the last
gate before the feature can be left on overnight.

The three switches and the running marker are documented in
[redis.md](redis.md); this page covers the guarantees they give and how
to verify them.

## Guarantees

### 1. At most one concurrent auto-executed job

The full-auto path of the agent-run loop runs every auto round with
`jarvis-jobs run … -auto`. That first takes the **`nobrakes:running`**
marker in Redis (value: the job id, no TTL) before the round starts and
releases it when the round ends, whatever the outcome:

- **Marker free** → the round runs and holds the marker for the whole
  round.
- **Marker held by a job that is still `running`** in the job store →
  `run -auto` does not run the job; it prints the holder and exits **3**.
  The loop sees exit 3 and idle-polls (60 s) instead of starting a second
  auto job.
- **Marker held by a job that is no longer running** (crash, reboot, a
  round that never released) → the marker is *stale* and is taken over,
  so a stuck marker cannot wedge the feature. The takeover is checked
  back against the key after the write, so two contenders racing on the
  same stale marker cannot both win.
- **No Redis** (disabled or unreachable) → `run -auto` refuses to run at
  all. An unguarded auto run is exactly what the guard exists to
  prevent, so it fails loud, not quiet.

The marker has no TTL on purpose: the guarantee must not depend on a
clock. A marker that is stuck *and* whose holder still reads as running
(the only case the takeover cannot fix) is recoverable by hand:
`jarvis-jobs running` shows the holder, `jarvis-jobs running clear`
removes it. Until then the auto path just idle-polls — the fail state
is "no auto work", never "two auto jobs".

### 2. An auto-job error pauses the feature and notifies the owner

When an auto round ends with the job `failed`, the loop calls
`jarvis-jobs autopause <id>`, which:

1. **Pauses**: sets `nobrakes:auto_mode` **and** `nobrakes:full_auto` to
   `off`. Both are turned off — the job says "set auto_mode off", but
   leaving `full_auto` on would let the loop keep grabbing the next
   ready job, which is not what "pause" means.
2. **Notifies**: DMs the owner on Discord with the job title, id and
   error, e.g.

   > NoBrakes: auto job "Fix the flaky test" (job-x) failed: agent
   > exited with status 1. Autonomous execution is paused — auto_mode
   > and full_auto are now off; re-enable with `!automode on` /
   > `!fullauto on`.

   The notification goes out **even when Redis is down**: a dead or
   missing flag already reads as `off` in the loop, so the system is
   safe either way, but the owner must still hear that a job failed.
   Only a Discord failure makes `autopause` itself fail.

Re-enabling is a deliberate operator act (`!automode on` / `!fullauto
on`). The feature never re-enables itself.

### 3. Manual override always works

Nothing in this layer can stop a human:

- **Supervised rounds** (full_auto `off` — the default): the loop asks
  on Discord and runs whatever the owner picks. They never touch the
  running marker, are never blocked by it, and never trip the
  pause-on-error path (a `!pause` or `!restart` mid-run is an operator
  act, not an auto failure).
- **`jarvis-jobs run <id>` without `-auto`** runs any job directly,
  marker untouched, regardless of any flag.
- **`!automode` / `!fullauto`** (Discord) and the brain's
  **`set_automode` / `set_fullauto`** actions (natural language: "turn on
  full auto") write the flags straight into Redis from the chat daemon —
  they never pass through a job or a marker, so they always work, even
  mid-round. A mode change is never routed to `enqueue_job`: the
  classifier is told the switches are the only way to change a mode.
- **`!pause` / `!restart`** still interrupt a running auto round.

### 4. Fail-safe defaults

Every NoBrakes read fails safe: a missing key, a corrupt value, or an
unreachable Redis reads as `off` / "no marker". Turning the feature on
always requires an explicit operator write that succeeds.

## On/off semantics

| Flag | On | Off (default) |
|---|---|---|
| `nobrakes:auto_mode` | the nightly gate auto-enqueues parked nightly jobs once per UTC day | nightly jobs stay parked until enqueued manually (`jarvis-jobs enqueue`) |
| `nobrakes:full_auto` | the loop runs the next ready job immediately, at any time, guarded by the running marker | the loop asks the owner on Discord who runs the next job |
| `nobrakes:running` | set by `run -auto` for the duration of one auto round | (absent) no auto job is running |

The two switches are independent and can be combined (nightly batch +
full auto). Either being `on` means "autonomous execution may happen";
an auto failure turns **both** off. The marker is not a switch — it is
the concurrency guard on the execution itself.

## How to test

**Unit tests** (miniredis, no live services):

```bash
go test ./internal/nobrakes/ ./cmd/jarvis-jobs/
```

- `internal/nobrakes/running_test.go` — marker acquire (free / held /
  reentrant / stale takeover), release by holder vs. non-holder,
  holder/clear, `Pause` turning both switches off.
- `internal/nobrakes/wake_test.go` — `WakeLoop`: no-op when the loop is
  active, starts a stopped loop via the (stubbed) `agent-run-restart`
  helper, and the owner-facing notes for the inhibited / resume-pending /
  helper-failure cases.
- `internal/hermes/automode_test.go` — the `set_automode` /
  `set_fullauto` actions flip the right flag (and only that flag), confirm
  with the shared text, append the wake note when the loop was stopped,
  and answer owner-facing no-change messages when Redis is disabled or
  unreachable.
- `internal/hermes/fast_classify_test.go` — "turn on full auto" style
  requests classify to the mode actions with the state extracted; state
  questions ("is full auto on?"), statements, and negated requests never
  flip a flag on the fast path.
- `cmd/jarvis-chat/{automode,fullauto}_test.go` — the `!` commands
  (miniredis), including the loop-wake note on `!fullauto on` and the
  loop-stopped note on `!automode on`.
- `cmd/jarvis-jobs/guard_test.go` — `autopause` (pause + notify,
  notify-out when Redis is disabled/unreachable, notify failure is an
  error), `running` show/clear, `run -auto` refusing without Redis and
  reporting a live holder (exit 3 in the binary).

**Live smoke** (feature stays off; each step restores state):

```bash
# 1. Marker is absent while nothing auto runs:
jarvis-jobs running                                   # (none)

# 2. A held marker blocks a second auto run (exit 3) — hermetic, in a
#    scratch job store, so no real round starts and no real state moves:
scratch=$(mktemp -d)
JARVIS_DATA_DIR=$scratch jarvis-jobs add -id held -title "smoke: held" -source smoke
JARVIS_DATA_DIR=$scratch jarvis-jobs add -id other -title "smoke: other" -source smoke
printf '{"status":"running"}' > "$scratch/jobs/held/state.json"   # holder is live
docker exec jarvis-redis redis-cli SET nobrakes:running held
JARVIS_DATA_DIR=$scratch jarvis-jobs run other -auto; echo $?     # 3 — job untouched
# and the same marker is NOT held when the holder is dead (stale):
printf '{"status":"done"}' > "$scratch/jobs/held/state.json"
# (do NOT run `run other -auto` here — a stale marker is taken over and
# the round would really start; the takeover is unit-tested instead)
docker exec jarvis-redis redis-cli DEL nobrakes:running
rm -rf "$scratch"

# 3. An auto failure pauses + notifies (watch Discord):
#    queue a job that fails, !fullauto on, wait for the
#    "NoBrakes: auto job … failed … paused" DM, then:
jarvis-jobs fullauto                                  # off
jarvis-jobs running                                   # (none) — marker released
# re-enable when you want to go on:  !fullauto on

# 4. Manual override: with full_auto on, a supervised-style manual run
#    of another job is unaffected, and !automode / !fullauto always
#    answer (they write Redis directly, never through a job).
```

**The invariant to trust:** if any part of the guard is broken in a way
the tests don't cover, the system degrades to "no auto work happens"
(idle-polling, refused `run -auto`, flags reading off) — it cannot
degrade to "two auto jobs at once" or "silently keeps running after a
failure".
