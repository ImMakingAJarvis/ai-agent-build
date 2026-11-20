---
id: home-assistant
name: Home Assistant
summary: The house (111.1.2.200) — voice satellite, MQTT sensors, lights and devices; reachable by MCP, REST and WebSocket.
group: external
kind: external
status: external
ports: ["80 (HA behind a proxy at 111.1.2.200)"]
docs: [docs/agent-claude.md, docs/wakeword-pulse-then-stop-diagnosis.md]
depends_on:
  - to: voice
    why: "the jarvis_voice integration posts conversation turns"
  - to: rabbitmq
    why: "subscribes to MQTT 1883 for the Jarvis sensors"
---

## Purpose

HA 2026.x on its own box. The Voice Preview satellite does wake word and
STT and speaks replies with HA's own TTS; the hand-installed
`jarvis_voice` integration sends conversation turns to the voice service.
The agent drives HA through the `homeassistant` MCP server.

## Interfaces

- MCP `http://111.1.2.200/api/mcp`, REST `/api/*`, WebSocket
  `/api/websocket`.
- No SSH or Supervisor access from this box: add-on options (Piper,
  STT engine) need Nathan or the HA UI.

## Configuration

- Long-lived token `/home/nathan/HAtoken.txt` (sudo; never print).

## Data

- Lives on the HA box.

## Deploy

Not deployed from this repo.

## Verify live

Use the MCP `GetLiveContext` tool, or
`curl -s -H "Authorization: Bearer $(sudo cat /home/nathan/HAtoken.txt)" http://111.1.2.200/api/ | head -c 100`.

## Failure modes

- The Assist pipeline's STT (`stt.speech_to_phrase`) dead since ~10-23:
  wake word fires, then nothing (diagnosed 8 times, 10-26/27; needs the HA
  box).
- The satellite speaks via google_translate, not Piper — tuning Piper did
  nothing (2026-11-16).
