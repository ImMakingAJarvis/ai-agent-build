---
id: superagent
name: Super Agent
summary: A plan→act→verify agent loop on the local brain, usable as a job driver.
group: agent
kind: cli
status: active
binaries: [cmd/superagent]
ports: ["— (CLI)"]
docs: [docs/superagent.md]
depends_on:
  - to: engine
    why: "every plan/act/verify step is a brain call"
  - to: memory
    why: "memory_search tool"
  - to: rabbitmq
    why: "rabbitmq tool (caps, submit, listen, yield)"
  - to: jarvis-jobs
    why: "--job mode reads the job and finishes it"
---

## Purpose

DeepSeek-style super agent (2026-11-04, PR #147): plans in JSON,
executes steps through a text `<ACTION>{tool,args}</ACTION>` protocol,
verifies, replans. A Guard blocks catastrophic commands and confines paths
to the work dir. Selected with `-driver superagent`.

## Interfaces

- `superagent "goal" | --prompt-file F | --job <id>`; exit 0 iff done.
- Tools: shell, read, write, list, memory_search, web_fetch, rabbitmq,
  step_done, replan, finish.

## Configuration

- `SUPERAGENT_MODEL` pins a different engine model (default: the brain).
- Step/turn/replan/timeout bounds are flags (docs/superagent.md).

## Data

- Works inside its work dir; no state of its own.

## Deploy

    go build -o /tmp/superagent ./cmd/superagent && sudo install -m 755 /tmp/superagent /usr/local/bin/superagent

## Verify live

    superagent "write the word ok to /tmp/sa-check.txt" && cat /tmp/sa-check.txt

## Failure modes

- None recorded since 2026-11-04.
