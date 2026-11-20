# Service specs

One file per service, component or external system the box depends on:
`specs/<id>.md`. The spec is the contract a round reads **before** it
changes a service and updates **in the same PR** when it changes one.

Why this exists: most repeat fixes in the 2026-10-26 → 11-19 record were a
round not knowing what a service talks to, where it runs, or how to deploy
and check it — fixed on the host for code that runs in a container, merged
and never deployed, verified by a unit test and not on the running system.
The spec is where that knowledge lives, one place per service.

The dashboard's **System map** (`/map` on :8080, linked from the queue
site) is drawn from these files: every spec is a node, every `depends_on`
entry an edge. The files are embedded in the binary, so the map matches
the commit that was deployed.

## The rules

1. **Every compose service, every `systemd/*.service|*.timer` unit and
   every `cmd/<name>` package is claimed by exactly one spec.** A spec
   that names something which no longer exists is an error too.
   `go test ./specs/` enforces both directions — so does `go test ./...`.
2. **New service → new spec in the same PR.** Copy `_template.md`.
3. **Changed service → updated spec in the same PR** when the change
   touches anything the spec states: ports, env vars, dependencies, data
   paths, how to deploy, how to verify, a new failure mode you hit.
4. **Read the spec before you touch the service.** It names the deploy
   command and the live check; use them.
5. **Facts only.** If you do not know something, leave it out — do not
   guess. A wrong spec is worse than a short one.
6. **No secrets.** Name env files and variables; never values.

## Front matter (machine-readable)

```yaml
---
id: dashboard                 # = file name; lowercase, digits, dashes
name: Dashboard (queue site)  # display name
summary: one line — what it is for (shown on the map)
group: face                   # map lane, see below
kind: container               # container | host-service | timer | cli | external
status: active                # active | optional | disabled | external
compose: [dashboard]          # compose services this spec covers
units: [jarvis-dashboard.service]   # systemd/ unit files it covers
binaries: [cmd/dashboard]     # cmd/<pkg> packages, or script paths
ports: ["8080 (LAN)"]         # free text, one per port
health: http://dashboard:8080/   # probed FROM the dashboard container;
                              # http:// (2xx/3xx = up) or tcp://host:port;
                              # leave it out when there is no probe
docs: [docs/dashboard.md]     # repo-relative, must exist
depends_on:                   # what THIS service calls / reads / needs
  - to: redis
    why: "live job streams"
---
```

Write each `depends_on` entry in the block form above with `why` in
double quotes — a comma or colon inside an unquoted one-line
`{to: x, why: a, b}` breaks the YAML (the parser rejects unknown fields,
so the test catches it).

Groups (map lanes, left → right): `external`, `face`, `agent`, `board`,
`plumbing`, `resources`, `capability`, `brain`, `media`, `house`,
`memory`, `monitoring`.

`health` is written as the dashboard container sees the network: compose
service names (`http://memory:8101/health`), the iGPU by container name,
host services through `host.docker.internal`.

## Body (required sections, in this order, none empty)

- `## Purpose` — what it does and why it exists, in a few lines.
- `## Interfaces` — ports, HTTP endpoints, queues consumed/published,
  CLI commands, Discord commands. What other things call.
- `## Configuration` — env vars and files it reads (names, not values),
  and where they come from (compose, `agent.env`, unit file).
- `## Data` — what it stores and where; what is safe to delete.
- `## Deploy` — the exact commands that put a change live, and how to
  confirm the running thing is the new one.
- `## Verify live` — the exact checks that prove it works on the running
  system, run from where the code runs (inside the container for a
  container).
- `## Failure modes` — what has broken before and how it showed up, with
  dates or PR numbers when known.
