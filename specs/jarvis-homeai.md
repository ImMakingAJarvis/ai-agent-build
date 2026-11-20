---
id: jarvis-homeai
name: HomeAI announcements (jarvis-homeai)
summary: Presence-aware TTS announcements through Home Assistant; inert until HA_URL/HA_TOKEN are set and the binary is installed.
group: house
kind: cli
status: disabled
binaries: [cmd/jarvis-homeai]
ports: ["— (CLI)"]
docs: [docs/system-frame.md]
depends_on:
  - to: home-assistant
    why: "presence check and TTS service calls"
---

## Purpose

Cherry-picked 2026-11-08 (EVB-6, Nathan: keep). `announce "message"`
checks the announcement switch and presence, then speaks through HA;
`presence` prints home/not_home. agent-run calls the announce hook only
when the binary exists and HA is configured.

## Interfaces

- `jarvis-homeai announce "msg" | presence`.
- HA YAMLs under `integrations/jarvis_homeai/`.

## Configuration

- `HA_URL`, `HA_TOKEN` (not set today — the feature is inert).

## Data

- None.

## Deploy

    go build -o /tmp/jarvis-homeai ./cmd/jarvis-homeai && sudo install -m 755 /tmp/jarvis-homeai /usr/local/bin/jarvis-homeai

## Verify live

    jarvis-homeai presence

## Failure modes

- None recorded — it has not been turned on.
