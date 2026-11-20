---
id: jarvis-watchdog
name: Watchdog
summary: Host failsafe — restarts a dead brain, iGPU, broker or worker, watches agent.log for round deaths, escalates to Discord.
group: agent
kind: host-service
status: active
units: [jarvis-watchdog.service]
binaries: [scripts/jarvis-watchdog, scripts/install_watchdog.sh]
ports: ["— (no listener)"]
docs: [docs/worker-moe-incident-2026-11-04.md]
depends_on:
  - to: engine
    why: "probes :11434 and restarts the container"
  - to: engine-igpu
    why: "probes :11435 and restarts the container"
  - to: rabbitmq
    why: "watches the broker container"
  - to: worker
    why: "watches the worker container"
  - to: agent-run
    why: "watches the unit state and tails agent.log"
  - to: discord
    why: "escalations"
---

## Purpose

Shipped 2026-11-05 after the brain was dark for 5 hours. Every 60 s:
probe the brain, iGPU helper, engine/rabbitmq/worker containers and the
agent-run unit; restart what is dead, at most 3 restarts per component per
hour, then escalate to Discord and cool down. Tails agent.log for round
deaths carrying the ECONNREFUSED signature. Escalates deployment
conflicts instead of auto-healing them.

## Interfaces

- `jarvis-watchdog --once | status | maintain <dur> | maintain off`
- Maintenance window: a TTL during which brain restarts are skipped; it
  re-arms the brain when it expires.

## Configuration

- Unit `systemd/jarvis-watchdog.service`, runs as `agent`.

## Data

- `~/.local/state/jarvis/watchdog/` state + `watchdog.log`.

## Deploy

    scripts/install_watchdog.sh && sudo systemctl restart jarvis-watchdog

## Verify live

    jarvis-watchdog status
    tail -n 20 ~/.local/state/jarvis/watchdog.log

## Failure modes

- A swap window is a legitimate brain-down — the window opens a
  maintenance window first; without it the watchdog fights the window.
- Before the docker-only change the watchdog resurrected a disabled host
  unit during a cutover (2026-11-08).
