---
id: redis
name: Redis
summary: Live job output streams and the NoBrakes mode flags; also the !fixnow resume context.
group: plumbing
kind: container
status: active
compose: [redis]
ports: ["6379 (127.0.0.1; compose name redis)"]
health: tcp://redis:6379
docs: [docs/nobrakes/redis.md, docs/nobrakes/safety.md]
depends_on: []
---

## Purpose

- Per-job streams `jarvis:job:<id>` (the runner XADDs every stream-json
  line; the dashboard tails them for the live view).
- NoBrakes flags: `nobrakes:auto_mode`, `nobrakes:full_auto`,
  `nobrakes:keep_going`, `nobrakes:nightly_flag`, `nobrakes:running`.
- `!fixnow` saves a cut-off round's transcript tail here (24 h TTL).

## Interfaces

- RESP on 6379. Host clients use `redis://127.0.0.1:6379`; containers
  must use `redis://redis:6379` (`JARVIS_REDIS`).

## Configuration

- `redis-server --appendonly yes --maxmemory 256mb --maxmemory-policy noeviction`.

## Data

- Named volume `redis_data` (AOF). Flags default to off when missing.

## Deploy

    docker compose up -d redis

## Verify live

    docker exec jarvis-redis redis-cli PING
    docker exec jarvis-redis redis-cli GET nobrakes:full_auto

## Failure modes

- A container without `JARVIS_REDIS` dials its own loopback and every
  flag write fails (chat, 2026-11-04, PR #155).
