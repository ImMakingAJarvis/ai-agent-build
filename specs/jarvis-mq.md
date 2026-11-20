---
id: jarvis-mq
name: jarvis-mq (event CLI)
summary: The round's door into the event system — submit capabilities, listen/yield, results, mailboxes, sync call, queue tools.
group: plumbing
kind: cli
status: active
binaries: [cmd/jarvis-mq, scripts/jarvis-review.sh, scripts/e2e-cap-echo.sh, scripts/e2e-event-flow.sh]
ports: ["— (CLI)"]
docs: [docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "every subcommand talks to the broker"
  - to: switchboard
    why: "listen/unlisten write the switchboard's listener store"
---

## Purpose

- Capability loop: `caps`, `submit <cap>`, `listen`, `unlisten`,
  `listeners`, `results`, `call` (sync RPC; refuses `needs_brain_out`).
- Mailboxes: `ask`, `tell`, `answer`, `inbox`, `take`.
- Queue tools: `publish`, `consume`, `peek`, `depth`, `queues`, `purge`,
  `delete`, `event`, `tail`.
- `jarvis-review request <PR>` hands a PR to a reviewer's inbox.

## Interfaces

- Publishes to `cap.*`, `flow.*`, `agent.*.inbox`, the `events` exchange.

## Configuration

- `RABBITMQ_URL` (host default `127.0.0.1:5672`), `CAPABILITIES_FILE`.

## Data

- Listener records under `~/.local/share/jarvis/listeners/`.

## Deploy

    go build -o /tmp/jarvis-mq ./cmd/jarvis-mq && sudo install -m 755 /tmp/jarvis-mq /usr/local/bin/jarvis-mq

## Verify live

    jarvis-mq caps
    jarvis-mq call echo --input msg=ok      # round trip through cap-echo

## Failure modes

- Flags after the subject were swallowed into it (fixed 2026-11-13).
- `answer` has no `--body`; findings land in the subject (2026-11-15).
- The installed binary printed the stub catalog until rebuilt (2026-11-13).
