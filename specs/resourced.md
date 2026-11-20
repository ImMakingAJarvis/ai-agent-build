---
id: resourced
name: resourced (VRAM ledger + swap windows)
summary: VRAM/RAM lease broker for the capability workers and the controller that swaps the brain out for photos, voice calls and games.
group: resources
kind: host-service
status: active
units: [resourced.service]
binaries: [cmd/resourced, scripts/install_resourced.sh, scripts/jarvis-swap]
ports: ["9104 on 172.17.0.1 (docker bridge; containers use host.docker.internal:9104)"]
health: http://host.docker.internal:9104/healthz
docs: [docs/brain-swap.md, docs/event-driven.md, docs/voice-call.md]
depends_on:
  - to: engine
    why: "stops and restarts the brain container through jarvis-swap"
  - to: art
    why: "brought up for a photo window"
  - to: bench
    why: "brought up for a photo window"
  - to: engine-voice
    why: "started for a voice call window"
  - to: rabbitmq
    why: "cap.* depths (mgmt API) and lease/window/stream events"
  - to: jarvis-jobs
    why: "running/ready local jobs decide when a window may open"
  - to: jarvis-watchdog
    why: "opens a maintenance window before stopping the brain"
  - to: agent-run
    why: "writes the restart-signal to cut a running round when a game needs the GPU"
---

## Purpose

- **Ledger**: the brain's standing GPU lease (measured live from
  nvidia-smi) plus worker leases. `POST /lease` grants when it fits,
  queues FIFO when it might, denies with a reason when it never will.
- **Window controller** (EVB-27, 30 s tick): opens a swap window when
  `needs_brain_out` work is queued and no local round runs, drains it,
  closes it (brain back) on drain / `MAX_WINDOW` / `MAX_BRAIN_WAIT`.
- **Game watchdog**: a Steam game revokes GPU leases and swaps the brain
  out; a Sunshine desktop stream does not.

## Interfaces

- `POST /lease`, `DELETE /lease/<id>`, `GET /state`, `GET /healthz`,
  `GET /metrics`, `POST /measure`, `POST/DELETE /window/voice`.
- `jarvis-swap out --for <caps> | --game | --voice`, `in`, `status`,
  `handoff`. Only resourced calls `out` between rounds.
- Events: `lease.*`, `window.*`, `stream.*`.

## Configuration

- Unit env (`systemd/resourced.service`): `LISTEN_ADDR`, `RABBITMQ_URL`,
  `CAPABILITIES_FILE`, `REFRESH_INTERVAL`, `WINDOW_INTERVAL`,
  `MAX_WINDOW`/`MAX_BRAIN_WAIT`/`MIN_WINDOW` (2h/20m/2m),
  `SUNSHINE_STREAM_PORT`, `STEAM_GAME_EXCLUDES`.
- Runs on the host: it needs docker, nvidia-smi and the job store.

## Data

- `~/.local/state/jarvis/swap-window.json` (window state; the dashboard
  reads it), `cap-measurements.json`, `resourced.log`.

## Deploy

    scripts/install_resourced.sh        # binary + unit + restart
    scripts/install_agent_run.sh        # installs jarvis-swap

The installer covers the binary only; `jarvis-swap` deploys separately
(2026-11-13: the old script rejected `--game`).

## Verify live

    curl -s 172.17.0.1:9104/state | head -c 400
    jarvis-swap status
    tail -n 20 ~/.local/state/jarvis/resourced.log

## Failure modes

- A stranded window left the brain down until its deadline (2026-11-06,
  11-07); stale queued messages burned a 2 h empty window (2026-11-12).
- Game black screen: the retry timer was never re-armed, so the brain kept
  the VRAM (2026-11-19, PR #267).
- The game detector matched a round's prompt text and stopped the brain in
  a loop (2026-11-19, PR #268).
- An unidentified caller started the engine mid-game (2026-11-19, noted in
  PR #267).
