---
id: think-timers
name: Idle-work timers (think queue)
summary: Keeps the think queue topped up while idle and sends an 08:00 digest of overnight think notes.
group: agent
kind: timer
status: active
units: [jarvis-think-topup.service, jarvis-think-topup.timer, jarvis-think-digest.service, jarvis-think-digest.timer]
binaries: [scripts/think_topup.py, scripts/think_digest.py, scripts/install_think.sh]
ports: ["— (timers)"]
docs: [docs/idle-work.md]
depends_on:
  - to: jarvis-jobs
    why: "enqueues think jobs into the agentjob store"
  - to: discord
    why: "the morning digest"
---

## Purpose

When the queue is empty the agent does "think" work (docs/idle-work.md).
`jarvis-think-topup` (every 15 min) keeps that backlog queued;
`jarvis-think-digest` (08:00) summarises the overnight think notes for
Nathan. Added 2026-11-18.

## Interfaces

- Two systemd timers and their oneshot services.

## Configuration

- Unit files in `systemd/`; scripts run with `/usr/bin/python3` from the
  repo checkout.

## Data

- Think notes and backlog as described in docs/idle-work.md.

## Deploy

    scripts/install_think.sh

## Verify live

    systemctl list-timers 'jarvis-think-*' --no-pager

## Failure modes

- New on 2026-11-18; nothing recorded yet.
