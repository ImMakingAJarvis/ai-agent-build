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
  line; the dashboard tails them for the live view). `MAXLEN ~ 5000`,
  6 h safety TTL while the round runs, 1 h TTL once it ends
  (`internal/agentjob/stream.go`).
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
- **Full at maxmemory.** With `noeviction`, a full Redis refuses every
  write (`OOM command not allowed`) while reads still work: full auto
  reads "on" but `jarvis-jobs run -auto` cannot take `nobrakes:running`
  and exits 2. Job streams at 20k entries / 24 h TTL (~7.5 MB each) did
  this on 2026-11-20 and full auto posted "Full auto: running ..." to
  Discord every second; the caps are now 5k / 1 h and agent-run backs off
  a job that cannot start. Check: `redis-cli INFO memory`
  (`used_memory` vs `maxmemory`); relief: `redis-cli CONFIG SET
  maxmemory 512mb` (runtime) or delete finished jobs' `jarvis:job:*`
  streams — the transcripts are the durable copy.
