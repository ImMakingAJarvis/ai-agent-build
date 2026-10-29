# agent-run (coding rounds)

`agent-run` is the Discord-checkpoint loop that runs Claude Code jobs. It is a systemd service, same shape as `jarvis-chat`.

Unit: `/etc/systemd/system/agent-run.service` (repo: `systemd/agent-run.service`). User `agent`. Starts on boot.

It shares Ollama with `jarvis-chat`. That is expected.

The unit uses `Restart=on-failure`, so a crash or kill (non-zero exit) comes back after 10s; a Discord **Stop** (exit 0) stays stopped.

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
