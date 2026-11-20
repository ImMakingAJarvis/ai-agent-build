---
id: jarvis-stack
name: Stack boot unit (jarvis-stack)
summary: Oneshot that runs docker compose up -d for the whole stack at boot.
group: agent
kind: host-service
status: active
units: [jarvis-stack.service]
binaries: [scripts/install_stack.sh]
ports: ["— (oneshot)"]
docs: [docs/services.md]
depends_on: []
---

## Purpose

Belt and braces over the containers' restart policies: at boot it runs
`docker compose -p ai-agent-build -f /home/agent/ai-agent-build/docker-compose.yml up -d`
as a oneshot with `RemainAfterExit=yes`.

## Interfaces

- `systemctl start jarvis-stack` re-runs `compose up -d` (also recreates
  deliberately stopped containers).

## Configuration

- The compose file at the canonical repo path; project name
  `ai-agent-build`.

## Data

- None.

## Deploy

    scripts/install_stack.sh

## Verify live

    systemctl status jarvis-stack --no-pager | head -5
    docker compose ps

## Failure modes

- Containers that dockerd starts before this unit can come up with no
  network (the 2026-11-05 zombie engine).
