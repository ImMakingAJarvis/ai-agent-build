---
id: jellyfin
name: Jellyfin (Reel baseline)
summary: Stock Jellyfin media server with NVENC — the day-1 Plex alternative for the Reel plan.
group: media
kind: container
status: active
compose: [jellyfin]
ports: ["8096 (LAN)"]
health: http://jellyfin:8096/health
docs: [docs/reel/jellyfin-baseline.md, docs/reel/decisions.md]
depends_on:
  - to: nas
    why: "real media library (not mounted yet — demo library in use)"
---

## Purpose

Reel P0 (2026-11-04, PR #151): Jellyfin 12.4 with 4090 NVENC transcoding,
Movies and TV libraries. Currently serves a demo library; the NAS share is
a later swap-in.

## Interfaces

- Web UI and API on :8096 (12.x API: no `/api` prefix,
  `Authorization: MediaBrowser ...` header).

## Configuration

- Config `/var/lib/jarvis/jellyfin/config`; admin creds in
  `~/.config/jarvis/jellyfin.env` (never committed).

## Data

- Config dir above; media read-only from `/var/lib/jarvis/jellyfin/demo`.

## Deploy

    docker compose up -d jellyfin

## Verify live

    curl -s 127.0.0.1:8096/health

## Failure modes

- The NAS share named in the plan did not exist; demo library kept
  (2026-11-04, 11-06).
