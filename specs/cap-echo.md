---
id: cap-echo
name: Echo capability worker (test)
summary: Fake capability that waits, writes an artifact and replies — proves the submit → yield → wake loop.
group: capability
kind: container
status: active
compose: [cap-echo]
binaries: [cmd/cap-echo]
ports: ["— (no listener)"]
docs: [docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.echo, replies on the request's flow queue"
  - to: resourced
    why: "asks for a lease before consuming (capworker SDK)"
---

## Purpose

The Phase 2 gate worker (EVB-17): waits `ECHO_DELAY` (30 s), writes an
artifact and a `.done-<corr>` marker, replies with a Result. Used by the
gate scripts and `jarvis-mq call echo` as a known-good round trip.

## Interfaces

- Consumes `cap.echo`; replies to the request's `reply_to`.

## Configuration

- `RABBITMQ_URL`, `RESOURCED_URL`, `CAPABILITIES_FILE`, `ECHO_DELAY`.

## Data

- Artifacts under the request's out dir in `~/.local/share/jarvis`.

## Deploy

    docker compose build cap-echo && docker compose up -d cap-echo

## Verify live

    jarvis-mq call echo --input msg=ok
    bash scripts/e2e-cap-echo.sh

## Failure modes

- None of its own; it is the probe that found the switchboard SIGSEGV
  (2026-11-09).
