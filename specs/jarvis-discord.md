---
id: jarvis-discord
name: jarvis-discord + jarvis-reply (Discord CLIs)
summary: How rounds and scripts talk to Nathan — notify, ask with buttons, image; jarvis-reply answers on the reserved chat slot.
group: face
kind: cli
status: active
binaries: [cmd/jarvis-discord, cmd/jarvis-reply]
ports: ["— (CLI)"]
docs: [docs/discord.md]
depends_on:
  - to: discord
    why: "DMs through the bot"
  - to: engine
    why: "jarvis-reply calls the brain on the chat slot"
---

## Purpose

- `jarvis-discord notify "msg" [--item REF]` — DM without waiting (board
  link appended with `--item`).
- `jarvis-discord ask "q" [-choices a,b]` — DM and block for the answer.
- `jarvis-discord image <path>` — send a picture.
- `jarvis-reply` — answers Nathan with one brain call pinned to the
  reserved chat slot (`CHAT_SLOT_ID`, default 1) while a round keeps the
  work slot.

## Interfaces

- Discord REST through the bot token.

## Configuration

- Bot token and owner id from `~/.config/jarvis/discord.env`.

## Data

- None.

## Deploy

Built and installed by `scripts/install_chat.sh` (host binaries in
`/usr/local/bin`).

## Verify live

    jarvis-discord notify "spec check: jarvis-discord works"

## Failure modes

- A send whose success was not confirmed was re-sent, so Nathan got it
  twice (2026-11-02).
