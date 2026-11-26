---
id: loopd
name: Loopd (event-loop runtime)
summary: The event-loop agent runtime daemon — schedules ticks for jobs with driver loop. Skeleton only for now.
group: agent
kind: container
status: optional
compose: [loopd]
binaries: [cmd/loopd, cmd/loopctl]
ports: ["9110 health (published on 127.0.0.1 only; LOOPD_ADDR, default :9110)"]
health: http://loopd:9110/healthz
---

## Purpose

The event-loop agent runtime daemon: schedules ticks for jobs with driver
loop. Skeleton only for now — the card ELR-6 (M1.1a) created the homes
(`runtime/`, `cmd/loopd`, `cmd/loopctl`) that the later ELR cards fill in.
Since ELR-7 it builds as a compose service behind the `loop` profile.

## Interfaces

- `GET /healthz` on `LOOPD_ADDR` (default `:9110`) — returns 200 and the
  text `ok`. Containers on the compose network reach it at
  `http://loopd:9110/healthz`.
- `loopctl version` — prints `loopctl dev`; any other argument prints
  `usage: loopctl version` to stderr and exits 1.

## Configuration

- `LOOPD_ADDR` — listen address, default `:9110`. Read by `cmd/loopd`.
- Compose profile `loop` — it never starts with `docker compose up -d`.
- `env_file: agent.env`, like the other agent services.

## Data

- None yet — the skeleton holds no state. The job store
  (`~/.local/share/jarvis`) is mounted into the container; later cards add
  the job-store fields and the message log (ELR-8, ELR-9).

## Deploy

    docker compose --profile loop build loopd && docker compose --profile loop up -d loopd

## Verify live

    docker compose --profile loop up -d loopd
    curl http://127.0.0.1:9110/healthz          # → ok
    docker logs --tail 20 jarvis-loopd          # loopd listening on :9110

Outside the profile it is expected to be absent (the map shows it down).

## Failure modes

- None yet — the service is a skeleton (ELR-6, 2026-11-25).
