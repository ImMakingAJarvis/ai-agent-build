---
id: agent-run
name: Agent loop (agent-run)
summary: The host loop that runs one queued job per round as a Claude Code session on the chosen driver.
group: agent
kind: host-service
status: active
units: [agent-run.service]
binaries: [scripts/agent-run, scripts/agent-local, scripts/agent-resume, scripts/agent-reboot, scripts/agent-run-restart, scripts/install_agent_run.sh, scripts/notes-stop-hook.sh, scripts/owner-message-hook.sh, scripts/brain-guard-hook.sh, scripts/gh-pr-wait, scripts/jarvis-stack-status, scripts/install_claude_md.sh]
ports: ["— (no listener)"]
docs: [docs/agent-run.md, docs/agent-claude.md, docs/nobrakes/safety.md]
depends_on:
  - to: jarvis-jobs
    why: "jarvis-jobs next/run/finish — the job store is its queue"
  - to: engine
    why: "local driver rounds run on the brain (ANTHROPIC_BASE_URL)"
  - to: engine-igpu
    why: "igpu driver rounds"
  - to: anthropic-api
    why: "sonnet and opus driver rounds"
  - to: discord
    why: "checkpoint questions and notifications (jarvis-discord)"
  - to: github
    why: "branches and PRs (ghapp-token, gh)"
  - to: redis
    why: "NoBrakes flags, live job stream, !fixnow resume context"
  - to: rabbitmq
    why: "mailbox peek, capability catalog, events"
---

## Purpose

"The agent". A bash loop (`/usr/local/bin/agent-run`, user `agent`) that
pulls the next job, asks Nathan on Discord who runs it (or, in full-auto,
uses the job spec's driver), and runs one Claude Code round through
`jarvis-jobs run`. Each round prompt = the stage-3 brief for the driver +
MACHINE SNAPSHOT + CAPABILITIES + MAILBOX + the job (plus CONTINUATION or
RESUME blocks when the job was parked or preempted).

## Interfaces

- Drivers: `local` (brain), `igpu`, `sonnet`, `opus`, `superagent`.
- Hooks in `.claude/settings.json`: notes Stop hook, owner-message hook
  (PreToolUse + Stop), brain-guard hook (PreToolUse, Bash).
- Signal files in `~/.config/jarvis/`: `restart-signal`, pause/stop;
  inhibit file `/var/lib/jarvis/agent-run.disabled`.
- `agent-run-restart [--now]` — restart now if idle, else after the round.

## Configuration

- Unit `EnvironmentFile`: `~/.config/jarvis/agent.env`; drop-ins in
  `systemd/agent-run.service.d/` (effort, output tokens).
- Briefs: `~/.config/jarvis/stage3-<driver>.txt`, written by
  `bootstrap.sh` (the source of truth for their text).
- Standing instructions: repo `CLAUDE.md` + `~/.claude/CLAUDE.md`
  (installed from `docs/agent-claude.md` by `scripts/install_claude_md.sh`).

## Data

- Log: `~/.local/state/jarvis/agent.log`.
- Last driver: `~/.config/jarvis/driver` (a reboot resumes on it).
- Job transcripts: `~/.local/share/jarvis/jobs/<id>/transcript.txt`.

## Deploy

    scripts/install_agent_run.sh      # installs agent-run, jarvis-swap, unit
    agent-run-restart                 # the running loop keeps the OLD script until restarted

A bash loop holds the old script's inode: installing a new script changes
nothing until the service restarts (2026-11-06). Brief changes: re-write
`~/.config/jarvis/stage3-*.txt` from `bootstrap.sh`.

## Verify live

    systemctl is-active agent-run
    tail -n 20 ~/.local/state/jarvis/agent.log
    jarvis-stack-status

After a brief change, read the next round's `transcript.txt` head and
confirm the new text is in the prompt.

## Failure modes

- Dispatched a sonnet-specced job on the local brain, which stopped its own
  engine (2026-11-04).
- Re-dispatched into a dead engine 11 times (2026-11-04) — no circuit
  breaker.
- Ran an old script after install (2026-11-06).
- Supervised dispatch asked "who runs it?" for photo jobs that run as code
  (2026-11-07, PR #191).
