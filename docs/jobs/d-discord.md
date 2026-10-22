# D1-D8 — Two-way Discord conversation

Read only the section for your job. Background, shared by all D jobs:

- `cmd/jarvis-discord` (`internal/chat/discord.go`, discordgo) is already a bot, not a webhook. `notify` uses REST only. `ask` opens a short-lived gateway session with `IntentsDirectMessages`, DMs `DISCORD_OWNER_ID`, and takes the owner's reply in that DM.
- Config is `DISCORD_BOT_TOKEN` and `DISCORD_OWNER_ID` in `~/.config/jarvis/discord.env`. Never log, print or commit them.
- `agent-run` relies on `jarvis-discord ask` for every checkpoint, so it must keep working after every job. Install binaries and scripts atomically (temp name, then `mv`).
- The conversation stays in Nathan's DM with the bot. Discord delivers message content in DMs without the privileged Message Content intent, so no portal change is needed. A guild channel (D1.3) needs that intent.

## D1.1 — Daemon with owner-only intake

- Note: C1.1-C1.4 built `jarvis-chat` (always-on daemon, typing, brain replies). Extend it rather than starting another daemon or reply path.
- New `cmd/jarvis-bot`: one gateway connection that reconnects on drop. Accept a `MessageCreate` only if the author ID equals `DISCORD_OWNER_ID`, the author is not a bot, and the channel is the owner DM. Drop everything else, logging no content. Allow 20 messages a minute and 4000 characters per message. For now, reply with a ✅ reaction only.
- Done when: filter tests with fake events pass (owner DM accepted; other user, bot, other channel and too-long rejected).

## D1.2 — `jarvis-bot.service`

- systemd unit with `User=agent`, `EnvironmentFile=~/.config/jarvis/discord.env`, `Restart=on-failure`, enabled at boot.
- Done when: it is active, survives a restart, a DM from Nathan gets ✅, and the journal has no token (grep for the token's first 10 characters).

## D1.3 — Optional guild channel

- If `DISCORD_CHANNEL_ID` is set, also accept owner messages in that channel, using `IntentsGuildMessages | IntentsMessageContent`. Document in `docs/discord.md` the manual steps for Nathan: turn on Message Content Intent; give the bot View Channel, Send Messages, Read Message History and Add Reactions; how to copy the channel ID.
- Done when: filter tests cover the channel case, and without the variable nothing changes.

## D2.1 — Daemon routes answers to open questions

- The daemon serves a 0600 Unix socket (`~/.local/state/jarvis/bot.sock`) where a client registers a question (text and choices) and waits for its answer. It routes replies and button clicks the way `internal/chat` does today: a Discord reply to the question message wins, otherwise the oldest open question. Text starting with `!` is never treated as an answer.
- Done when: routing tests pass (plain reply, `!status` with a question pending, button click, two questions with a Discord reply).

## D2.2 — `ask` uses the socket, with fallback

- `jarvis-discord ask` uses the socket when it exists and falls back to today's direct gateway path when it doesn't.
- Done when: with the daemon running, `ask -choices "yes,no"` returns a click and a typed reply; with it stopped, `ask` still works; an `agent-run` checkpoint works end to end.

## D3.1 — Command parser and `!help`

- `!` prefix, case-insensitive, quoted arguments. Typed commands: help, status, queue, jobs, failures, prs, pause, resume, add, cancel, move, reset, forget, context. An unknown command replies with help.
- Done when: table-driven parser tests pass.

## D3.2 — `!pause jobs` and `!resume jobs`

- The worker stops taking new jobs; jobs already running finish. Expose this through a state file the worker watches, or a local control endpoint. It survives restarts.
- Done when: a paused test job waits, then runs after resume, and the paused state survives a worker restart.

## D3.3 — `!pause build` and `!resume build`

- Writes or removes `~/.config/jarvis/paused`. `agent-run` waits before starting the next round while the file exists (with a Discord notice) and never kills a running round.
- Done when: a pause during a round lets that round finish, and the next round starts only after resume.

## D3.4 — Confirm buttons for risky commands

- A helper that asks Yes/No with buttons and is used by cancel, move and forget.
- Done when: tests cover yes, no and timeout.

## D4.1 — Shared status package

- Move `gatherState()` and history reading out of the dashboard into `internal/status` (backed by the J1 database if it exists). The dashboard uses it unchanged.
- Done when: the dashboard tests still pass, and there are status tests with fixtures.

## D4.2 — `!status`

- Show: the current job (ID, title, how long it has run, driver), paused flags, queue depths, Ollama health and loaded model, and VRAM used.
- Done when: formatting tests cover idle, busy and paused, and the live reply matches the dashboard.

## D4.3 — `!queue`, `!jobs [n]`, `!failures [n]`, `!prs`

- Replies are split under 2000 characters.
- Done when: formatting tests with fixtures pass, including the empty case, and each command answers within 3s live.

## D5.1 — `!add [voice|coding|background|agent] <text>`

- Publish through `internal/queue` with `source: "discord"` and `reply_to` (DM channel and message ID). Reply with the job ID.
- Done when: a test checks the message fields, and live the job appears on the dashboard.

## D5.2 — Results go back to Discord

- When a `source: discord` job finishes or fails, DM the trimmed result with its ID to `reply_to`.
- Done when: a test with a fake sender passes, and live `!add background say hi` gets its answer back.

## D5.3 — `!cancel <id>`

- Remove a pending job, after confirmation. A running or unknown job gets a clear message and nothing changes.
- Done when: tests cover pending, running and unknown jobs, and the other jobs keep their order.

## D5.4 — `!move <id> <queue|priority>`

- Reprioritize a pending job, keeping its ID and `created_at`, after confirmation.
- Done when: tests show the job lands in the target with the same ID, and the other jobs are untouched.

## D6.1 — Chat goes to the brain as a voice job

- Note: C1.1-C1.4 built `jarvis-chat` (always-on daemon, typing, brain replies). Extend it rather than starting another daemon or reply path.
- An owner message that is not a command and not an answer becomes a `voice` job with `source: discord` and `reply_to`, on `LOCAL_MODEL`. A paid API is used only for `!ask-claude …`, through the cost gate. Strip thinking output and split long replies.
- Done when: routing tests with a fake model pass, and live "hi" gets a reply.

## D6.2 — Status-aware prompt and busy handling

- The system prompt includes a short status snapshot and says chat can't run commands (point to `!help`). Show the typing indicator. After 20s, say "busy, queued behind the build agent". Time out after 5 min.
- Done when: timeout and busy tests pass, and live "what are you working on?" mentions the current job.

## D7.1 — Conversation store

- Turns (role, text, time, job ID) go in the J1 database (table `chat_turns`). Each chat request sends the last turns that fit a 6000-token budget.
- Done when: windowing tests pass, and turns survive a restart.

## D7.2 — Rolling summary

- When history exceeds the budget, a background job summarizes the older turns, and the summary is included in the prompt.
- Done when: a test checks the summary is inserted and the old turns drop out of the window.

## D7.3 — `!reset`, `!forget`, `!context`

- reset starts a new conversation and keeps the old one. forget deletes it after confirmation. context shows the turns and tokens in use.
- Done when: tests pass, and live "my favourite colour is green", then a restart, then "what's my favourite colour?" answers green; after `!reset` it doesn't know.

## D8.1 — Docs and smoke script

- `docs/discord-bot.md` covers commands, routing, where state lives and how to restart. `scripts/discord_smoke.sh` checks that the daemon, socket and worker are up, and runs the parser and status tests.
- Done when: the script passes on the box.

## D8.2 — Live checklist with Nathan

- Ask Nathan on Discord to try each command, chat with a restart in between, answer a checkpoint while the daemon runs, and send a message from another account (it must be ignored). Record the results in the PR.
- Done when: every item passes.
