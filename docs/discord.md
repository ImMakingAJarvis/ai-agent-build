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
- `!photoassist <prompt>` — photo with iterative prompt assistance.
- `!photocheck <prompt>` — generate, critique, improve the prompt, regenerate.

  **Attach an image to any of these** and the photo becomes the
  reference: the generation routes to the reference-capable model
  (FLUX.2 [klein]) and the result keeps the subject from the photo
  (e.g. a picture of you, sent with the message). See
  docs/art-model.md, "Reference photos".
- `!build <title…>` / `!fix <title…>` — queue a build/fix job.
- `!automode on` / `!automode off` — NoBrakes nightly switch (auto-enqueue
  of nightly jobs). If the loop is stopped when you turn it on, the reply
  says so. See docs/nobrakes/redis.md.
- `!fullauto on` / `!fullauto off` — NoBrakes full-auto switch (run every
  ready job immediately). Turning it on also starts the agent-run loop if
  it is stopped, so it takes effect right away. See docs/nobrakes/redis.md.
- `!keepgoing on` / `!keepgoing off` — NoBrakes keep-going switch (a failed
  auto job notifies without pausing autonomous execution). See
  docs/nobrakes/redis.md.

The full reference — daemon commands, plain-language (Hermes) actions,
checkpoint buttons, access levels — is in docs/system-frame.md §3.

## Replying to a worker message

When a bot message (a job notification, a photo, …) is on screen and you use
Discord's **Reply** on it, the reply goes to the worker, not the chat:
`jarvis-chat` ignores it entirely (no chat answer, no job is scheduled from
it), drops it in the agent's inbox — the running round picks it up via the
owner-message hook, an idle agent at its next round start, with the bot
message quoted for context — and puts a 👍 on your message so you know the
worker read it. Replies to *your own* messages, and to `jarvis-discord ask`
questions (buttons / ✅), behave as before.

  The same switches work in plain language through the brain ("turn on
  full auto") — `set_automode` / `set_fullauto`, never via a queued job.

Instead of exporting variables, you can put the same `KEY=value` lines in
`~/.config/jarvis/discord.env` (bootstrap.sh writes this file for you).
Exit codes: 0 answered, 1 error, 3 `-timeout` passed with no answer.
