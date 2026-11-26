---
id: loopd
name: Loopd (event-loop runtime)
summary: The event-loop agent runtime daemon — schedules ticks for jobs with driver loop. Skeleton only for now.
group: agent
kind: cli
status: active
binaries: [cmd/loopd, cmd/loopctl]
ports: ["9110 health (LOOPD_ADDR, default :9110)"]
---

## Purpose

The event-loop agent runtime daemon: schedules ticks for jobs with driver
loop. Skeleton only for now — the card ELR-6 (M1.1a) creates the homes
(`runtime/`, `cmd/loopd`, `cmd/loopctl`) that the later ELR cards fill in.

## Interfaces

- `GET /healthz` on `LOOPD_ADDR` (default `:9110`) — returns 200 and the
  text `ok`.
- `loopctl version` — prints `loopctl dev`; any other argument prints
  `usage: loopctl version` to stderr and exits 1.

## Configuration

- `LOOPD_ADDR` — listen address, default `:9110`. Read by `cmd/loopd`.

## Data

- None yet — the skeleton holds no state. Later cards add the job-store
  fields and the message log (ELR-8, ELR-9).

## Deploy

- Not in the compose stack yet: the `loopd` service (profile `loop`) lands
  on card ELR-7 (M1.1b). Until then:
  `go build -o /tmp/loopd ./cmd/loopd && /tmp/loopd`.

## Verify live

- Not running yet (no compose service). Once ELR-7 lands:
  `curl http://127.0.0.1:9110/healthz` → `ok`, and
  `docker logs --tail 20 jarvis-loopd` shows `loopd listening on :9110`.

## Failure modes

- None yet — the service is a skeleton (ELR-6, 2026-11-25).
