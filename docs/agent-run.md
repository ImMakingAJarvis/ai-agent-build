# agent-run (coding rounds)

`agent-run` is the Discord-checkpoint loop that runs Claude Code jobs. It is a systemd service, same shape as `jarvis-chat`.

Unit: `/etc/systemd/system/agent-run.service` (repo: `systemd/agent-run.service`). User `agent`. Starts on boot.

It shares Ollama with `jarvis-chat`. That is expected.

The unit uses `Restart=on-failure`, so a crash or kill (non-zero exit) comes back after 10s; a Discord **Stop** (exit 0) stays stopped.

## Drivers (who runs a round)

Each round runs on one of three drivers, picked in the checkpoint:

- **Local model** (default): `agent-local` on the 27B brain (port 11434).
- **Igpu model**: the same `agent-local` path pointed at the 3B helper
  on the Ryzen iGPU (port 11435) with a 32K context — no 4090, no API.
  Right choice for rounds that must free the GPU (MoE VRAM sweeps).
  `IGPU_MODEL` / `IGPU_URL` / `IGPU_CONTEXT_TOKENS` override the
  defaults. Details in docs/local-brain.md.
- **Sonnet**: the API `claude` binary, for rounds the local models are
  stuck on.

`jarvis-jobs run <id> -driver local|igpu|sonnet` does the same from the
command line; the driver of the last round is recorded in
`~/.config/jarvis/driver` (a reboot mid-round resumes on the same
driver).

## Photo window (jobs tagged `photo`)

Photo jobs need the 4090 **without** the brain on it: the art/bench
services wait for ~12 GB of free VRAM before generating
(docs/brain-swap.md). The loop brackets the whole ready batch with a
swap window instead of the old manual bracket (two extra igpu jobs):

- **Before the first photo job runs** the loop calls
  `jarvis-swap out --jobs <batch> --force`: watchdog maintenance window
  first (the brain re-arms at the deadline), brain unit stopped,
  art+bench up, and every ready photo job flipped to priority 1000 (the
  `igpu` driver label is vestigial for photo jobs). The pulled job then
  runs as **code** — the `jarvis-photo` executor, no model round (not
  even the iGPU helper), regardless of its spec driver.
- **`jarvis-jobs photo-ready`** lists the batch (ready + tagged, in run
  order; empty output = drained). The tag is set by image-queue
  ingestion or `jarvis-jobs add -tags photo`.
- **Re-flip:** `out` is idempotent — while the window is open, each
  photo pull re-flips the batch, so photo jobs that arrive mid-window
  join the priority-1000 batch and the maintenance window extends as
  long as there is work.
- **When the batch drains** (no ready photo jobs) the loop calls
  `jarvis-swap in` at the top of the next iteration: window closed,
  art+bench stopped, brain reloaded and API-checked.
- **Only windows the loop opened get closed by the loop:** a successful
  `out` writes the marker `~/.config/jarvis/photo-window`, and the
  drain check requires it. A manual window (docs/brain-swap.md) for
  non-photo work — e.g. a VRAM sweep with no photo jobs — never gets a
  marker and is left to the owner. A manual window *with* a photo batch
  is adopted: the first photo pull re-flips it and writes the marker.
- **Failure handling:** if `out` cannot open the window (e.g. brain down
  without a maintenance window — the 2026-11-04 mode the watchdog owns),
  the loop backs off 60 s and retries, notifying once per failure
  stretch, not per retry. A failed `in` is retried the same way; the
  watchdog deadline re-arms the brain either way. `jarvis-swap`
  notifies Discord on both a successful out and in; swap.log is
  `~/.local/state/jarvis/swap.log`.

## Restart from chat (`!restart`)

DM `!restart` to the bot while a round is running: `jarvis-chat` writes
`~/.config/jarvis/restart-signal`, the running job's signal watcher
(`agentjob.Runner`, polled once a second) kills the round and puts the job
back to `ready`, and the agent-run loop starts the same job over in a fresh
conversation. The bot acks the command, and the next checkpoint names the
re-queued job.

- The signal file is consumed by the watcher, and agent-run clears a stale
  one before each round, so a `!restart` sent between rounds cannot kill the
  next round.
- A late signal (after the round already finished) is a no-op: `Store.Restart`
  only acts on `running` jobs, so a done job is never re-run.
- `!restart` while a checkpoint question is open does nothing (the bot skips
  DMs then) — use the checkpoint's **Skip**/**Stop** buttons instead.

`!restart` restarts the *conversation*, not the service. To restart the
agent-run **service** itself use the restart helper below.

## Stopping a running job (park)

The job log page's **Stop work** button (or `jarvis-jobs stop <id>`) writes
`~/.config/jarvis/stop-signal`; the running job's signal watcher kills the
round and parks the job. A parked job is out of the active queue — `Next()`
never returns it — so the loop skips to the next work: in full auto the
next ready job starts immediately, in supervised mode the next checkpoint
asks about it. Re-queue a parked job with `jarvis-jobs requeue <id>`.

- Like `!restart`, the signal file is consumed by the watcher and a stale
  one is cleared before each round, so a stop sent between rounds cannot
  park the next job. A late stop is a no-op: `Store.Stop` only acts on
  `running` jobs.
- The runner polls the signal files under `$JARVIS_CONF` (default
  `~/.config/jarvis` when the variable is unset — the same directory
  `jarvis-chat` and `jarvis-discord` use to write theirs).

## Restart the service (`agent-run-restart`)

`/usr/local/bin/agent-run-restart` (repo: `scripts/agent-run-restart`,
installed by `install_agent_run.sh`) restarts the service from any shell:

    agent-run-restart            defer until the current round ends; else restart now
    agent-run-restart --now      restart immediately (kills a running round)
    agent-run-restart --dry-run  print the decision, do nothing

- **Mid-round** it does not kill the round: it arms the transient oneshot
  unit `agent-run-restart-later`, which polls `~/.config/jarvis/current-job`
  and restarts the service as soon as the armed round ends (1h timeout,
  after which it aborts). If a *different* job owns the loop meanwhile it
  aborts instead of killing the new round.
- **Down or idle** it restarts immediately — a restart of a dead unit
  starts it, so this is also the "daemon is down" trigger (it clears a
  stale `current-job` left by a dead round first).
- It respects the inhibit rules: it does nothing while
  `/var/lib/jarvis/agent-run.disabled` exists or `resume-pending` is set.

Entry points: `jarvis-jobs restart [--now]` (the jarvis-jobs interface;
works from any shell even when no round process is alive) and DM
`!restart-service` / `!restart-service now` (jarvis-chat runs the same
helper and replies with its decision).

## Inhibit (keep it down)

1. Home script (preferred): `~/disable-agent-run` writes `/var/lib/jarvis/agent-run.disabled` and stops the unit. Undo: `~/enable-agent-run`.
2. File only: `touch /var/lib/jarvis/agent-run.disabled` then `sudo systemctl stop agent-run`. Undo: `rm` the file and `sudo systemctl start agent-run`.
3. systemd: `sudo systemctl disable --now agent-run`. Undo: `sudo systemctl enable --now agent-run`.

Install: `sudo bash scripts/install_agent_run.sh` from a checkout of this repo.

`agent-resume.service` still handles `agent-reboot` (marker `~/.config/jarvis/resume-pending`). This unit does not start while that marker exists.
