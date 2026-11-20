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
- `!fixnow <title…>` — a fix that runs before anything else: interrupts the
  running round and queues the job at the top of the fix band. The cut-off
  job's context is saved (Redis, 24 h TTL) so it resumes right after the fix.
- `!igpu <task…>` — queue a task straight onto the **iGPU worker** (the
  small always-on model). The recovery command for when the 4090 brain is
  down or held for a game. See "`!igpu`" below.
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

## `!igpu` — a task for the iGPU worker

```
!igpu run `jarvis-swap status` and tell me what it prints
!igpu run `jarvis-swap in`, then `docker ps --filter name=jarvis-engine` and send me the output
```

`!igpu <task>` queues the task as an agent job that runs on the **igpu
driver**: an `agent-local` round on the 3B helper on the Ryzen iGPU
(`jarvis-engine-igpu`, port 11435 — docs/local-brain.md, "The igpu
driver"). It is for getting the system back when the 4090 brain is not
there to do it: resourced is holding the GPU for a game (rightly or
wrongly), a swap window is stuck, the engine crashed. In that state
`!build` / `!fix` jobs wait (they are driver-local, and `jarvis-jobs next`
skips them while the brain is down or the game hold is set) and plain
chat cannot schedule anything.

What it does not depend on:

- **No model call to parse it** — `jarvis-chat` handles the command
  itself, like `!fix`.
- **No RabbitMQ hop** — the job goes straight into the agentjob store
  (`~/.local/share/jarvis/jobs/`), the same path as `!build` / `!fix`.
- **No brain, no game hold** — the job's spec driver is `igpu`.
  `jarvis-jobs next` gates an igpu job only on the iGPU engine's
  `/health`; the game-hold marker, the brain's health, `jarvis-swap`'s
  round guard and resourced's window rules all look at driver-local
  rounds only.
- **No open question in the way** — it is the one command handled while
  a checkpoint question is open (the loop is usually parked on one when
  the brain is down). `agent-run` recognizes `!igpu …` typed at its
  question as a command, not as instructions for the job it was asking
  about (docs/agent-run.md).

What happens:

1. Owner-only, like every DM command: only the `DISCORD_OWNER_ID` DM
   reaches the command handler.
2. The job is stored with driver `igpu`, class `fix`, priority 1000 (top
   of the fix band, level with `!fixnow`) and the tag `igpu-direct`. The
   reply gives the **job id**, says whether the agent-run loop is up, and
   warns if the iGPU engine is not answering `/health` (the job then
   waits for it).
3. It is the next job the loop takes. It does **not** interrupt a round
   that is already running — it runs after it. (A game hold has already
   cut any running local round; to cut a stuck one yourself, `!restart`.)
   Full auto runs it on its spec driver; the supervised loop sees the
   `igpu-direct` tag and runs it on the iGPU model without asking "Who
   runs it?".
4. The result comes back the way every job reports: 🦜 on your message
   when the round picks it up, then one DM from the round — `igpu job
   <id> done: …` or `igpu job <id> FAILED: …` — and the loop's own
   end-of-round summary (supervised) or failure notice (full auto).

What the worker can do, and the limits (as of 2026-11-19):

- It **is** a tool-using round: Claude Code on the iGPU model, with a
  shell, running as the `agent` user. It can run `jarvis-swap`,
  `docker`, `jarvis-jobs`, `systemctl status`, read logs. The same
  setup runs the manual swap-window bracket jobs (docs/brain-swap.md).
- It is a **3B model with a 32K context**. Give it literal commands
  ("run X, then Y, send me the output"), not a diagnosis to work out.
  "Figure out why the brain is down and fix it" is beyond it; it will
  more likely report a failure than repair anything. The job wrapper
  tells it to do exactly the task, try a failing command at most twice,
  and report — not to improvise.
- It runs as the `agent` user with the same rights as every agent
  round: docker, and passwordless sudo (bootstrap.sh). So a task like
  "run `sudo systemctl restart resourced`" works — and a 3B model
  holding that shell is the reason to keep tasks literal. Nothing new
  is granted: this is what an igpu-driver round already had.
- If resourced is holding the GPU for a game that **is** running, a
  brain the task restarts is stopped again by resourced's keepalive
  within a tick. `!igpu` gets around a hold that is wrong (nothing is
  actually using the GPU); it does not overrule one that is right.
- A failed round in full auto pauses autonomous execution like any
  failed auto job (unless `!keepgoing on`).
- If the iGPU engine itself is down, the job waits (the reply warns
  about it). `!igpu` cannot fix that one: start the engine on the box
  (`docker start jarvis-engine-igpu`).
- If the agent-run loop is stopped, the job waits too; the reply says
  so, and `!restart-service` starts the loop.

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
