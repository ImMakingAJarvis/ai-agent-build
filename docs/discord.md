# Discord bot setup

The agent DMs you through a Discord bot: `jarvis-discord notify` sends a
message, `jarvis-discord ask` sends a question and waits for your answer.
The same code (`internal/chat`) becomes the Phase 7 bot later.

## One-time setup

1. Go to https://discord.com/developers/applications, click **New
   Application**, and name it (e.g. Jarvis).
2. **Bot** tab: click **Reset Token** and copy the token. That's
   `DISCORD_BOT_TOKEN`. Leave every Privileged Gateway Intent off; DMs
   don't need them.
3. Discord only lets a bot DM you if you share a server. Create a private
   server just for you (the **+** in the server list), then on the
   **OAuth2** tab tick the `bot` scope, open the generated URL, and add the
   bot to that server.
4. In Discord, open **User Settings → Advanced** and turn on **Developer
   Mode**. Then right-click your own name and choose **Copy User ID**.
   That's `DISCORD_OWNER_ID`. The bot ignores everyone else.

## Try it

```bash
export DISCORD_BOT_TOKEN=... DISCORD_OWNER_ID=...
go run ./cmd/jarvis-discord notify "hello from Jarvis"
go run ./cmd/jarvis-discord ask -choices "yes,no" "Does this work?"
```

`ask` prints whichever button you click, or whatever you type back. With
several questions open at once, use Discord's **Reply** on the question you
are answering; a plain message goes to the oldest open question. A ✅
reaction means your reply was received.

## Commands

The `jarvis-chat` daemon handles a few DM commands directly (no brain
involved):

- `!pause` — interrupt the running coding round and answer you first (the
  job is re-queued).
- `!restart` — kill the running round and re-queue its job so the agent run
  starts over in a fresh conversation. See docs/agent-run.md.
- `!restart-service` / `!restart-service now` — restart the agent-run
  *service* (defers until the current round ends unless `now`). Works when
  the service is down. See docs/agent-run.md.
- `!photo <prompt>` — generate a photo with the local art model.
- `!build <title…>` / `!fix <title…>` — queue a build/fix job.

Instead of exporting variables, you can put the same `KEY=value` lines in
`~/.config/jarvis/discord.env` (bootstrap.sh writes this file for you).
Exit codes: 0 answered, 1 error, 3 `-timeout` passed with no answer.
