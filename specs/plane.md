---
id: plane
name: Plane (work board)
summary: Self-hosted Plane CE on the NAS (111.1.2.222:8088) — one project per epic; the board the agent updates.
group: external
kind: external
status: external
ports: ["8088 (111.1.2.222)"]
docs: [docs/work-board/NOTES.md, docs/plane-to-queue.md]
depends_on:
  - to: plane-bridge
    why: "sends webhooks for board changes"
---

## Purpose

The work board (workspace `jarvis`). Standard since 2026-10-31: one
project per epic, created only with `plane-epic create`.

## Interfaces

- API v1 with `X-Api-Key`; the session API (pages, webhooks) needs a bot
  sign-in. Browse links: `http://111.1.2.222:8088/jarvis/browse/<REF>`.

## Configuration

- `PLANE_URL`, `PLANE_API_KEY`, bot credentials in `agent.env`.

## Data

- On the NAS.

## Deploy

Not deployed from this repo (Nathan runs it on the NAS).

## Verify live

    jarvis-board item <REF>

## Failure modes

- 429 rate limits on bulk writes.
- A plain member add grants viewer; editors need role 20.
