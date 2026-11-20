---
id: memory-reindex
name: Memory reindexer
summary: Re-scans every memory source every 15 minutes.
group: memory
kind: container
status: active
compose: [memory-reindex]
units: [jarvis-memory-reindex.service, jarvis-memory-reindex.timer]
ports: ["— (loop)"]
docs: [docs/agent-notes.md]
depends_on:
  - to: memory
    why: "writes the same memory.db"
---

## Purpose

A shell loop in the memory image: wait 3 minutes after start, then run
`memory_service.py --reindex` every 15 minutes. Replaced the
`jarvis-memory-reindex.timer` host unit (now legacy, disabled).

## Interfaces

- None; it only writes the database.

## Configuration

- Same image and mounts as `memory`.

## Data

- `~/.local/share/jarvis/memory/memory.db`.

## Deploy

    docker compose up -d memory-reindex

## Verify live

    curl -s 127.0.0.1:8101/stats      # last reindex time moves every 15 min

## Failure modes

- The repo walk must skip `.worktrees`, and a zero-file pass must never
  prune (fixed 2026-11-12).
