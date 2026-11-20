---
id: anthropic-api
name: Anthropic API (Claude)
summary: Paid cloud models — sonnet/opus round drivers, the reviewer, opt-in Fable planner. Never a default.
group: external
kind: external
status: external
ports: ["— (cloud)"]
docs: [docs/agent-run.md, docs/event-driven.md]
depends_on: []
---

## Purpose

Cloud models used only when the job spec or Nathan names them: `sonnet`
and `opus` round drivers, the code reviewer (`jarvis-review`), and the
registry's opt-in cloud planners (Fable only when an epic is labelled
`plan:claude-fable-*`).

## Interfaces

- The `claude` binary for driver rounds; HTTPS API from the worker's
  planner/reviewer.

## Configuration

- `ANTHROPIC_API_KEY` in `agent.env` (never printed).

## Data

- None on this box.

## Deploy

Not deployed from this repo.

## Verify live

Run a sonnet round on a trivial job, or check the reviewer job's
transcript for a 200.

## Failure modes

- A dead key failed ~60 queued API-driver jobs in seconds (401, found
  2026-11-15) — the loop kept dispatching them.
