---
id: discord
name: Discord
summary: Nathan's main channel — owner DMs to the bot, checkpoint questions, notifications.
group: external
kind: external
status: external
ports: ["— (cloud, WSS gateway + REST)"]
docs: [docs/discord.md]
depends_on: []
---

## Purpose

The face Nathan uses most. One bot token serves the `chat` daemon and the
`jarvis-discord` CLIs. Only owner DMs are acted on.

## Interfaces

- Gateway (WSS) for `chat`; REST for `jarvis-discord` notify/ask/image.

## Configuration

- Bot token and owner id in `~/.config/jarvis/discord.env` (never
  printed or committed).

## Data

- Conversation logs are kept on this box: `notes/conversations/discord-<date>.md`.

## Deploy

Not deployed from this repo.

## Verify live

    jarvis-discord notify "spec check"

## Failure modes

- Two processes on the same token double-reply and double-enqueue
  (2026-10-28) — run only the container.
