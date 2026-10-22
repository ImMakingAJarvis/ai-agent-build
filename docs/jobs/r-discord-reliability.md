# R — Reliable, responsive direct comms over Discord

Read only the section for your job. Background, shared by all R jobs:

- These come after D1 (bot daemon) and D2 (`ask` through the daemon), and before the D3+ commands.
- Goals:
    - Nathan never loses a message.
    - Every message he sends is acknowledged within about 1s.
    - Commands answer within about 2s.
    - Chat gets a first real reply within about 10s, even while a build round is using the GPU.
    - He can talk to the round that is running right now.
- The brain runs one sequence at a time (Ollama runner `-np 1`), and the card has only about 1 GB of VRAM free with the brain loaded (Oct 21). So chat queued behind a build round can wait minutes, which R1.5 addresses.
- Use the J1 database for all state and record events as in E1.5. Check current discordgo and Claude Code docs rather than assuming behaviour.

## R1.1 — Survive disconnects without losing messages

- Store the last processed DM message ID in the database. On every connect and resume, fetch owner DMs newer than it (`ChannelMessages(after=…)`) and process them in order. Deduplicate by message ID so nothing is handled twice. Record reconnects as events.
- Done when: tests with a fake session cover a gap, a duplicate, and ordering. Live: stop the bot, send three DMs, start it, and all three are handled once, in order.

## R1.2 — Outbox with retry

- Every outgoing message (notify, replies, questions) goes through a database outbox. A sender delivers it, splits anything over 2000 characters, honours 429 `retry_after`, retries with backoff for up to 24h, and uses a nonce so a retry never double-posts. `jarvis-discord notify` writes to the outbox and returns at once; with `--wait` it blocks until delivered.
- Done when: tests cover 429, a network error, a split, and no duplicate on retry. Live: messages queued during a 60s outage are delivered after it.

## R1.3 — Watchdog and a "bot down" alert

- `jarvis-bot.service` uses `WatchdogSec=60` with sd_notify heartbeats, sent only while the gateway is healthy. There is a `/healthz` on localhost. A separate systemd timer checks it every minute; after 2 minutes down it sends Nathan a REST-only notify ("Jarvis bot is down since …"), which needs no gateway. It sends one more message when the bot recovers.
- Done when: killing the gateway (fake hang) gets the service restarted, and a longer outage sends exactly one down and one up message.

## R1.4 — Instant acknowledgement

- Note: C1.1-C1.4 built `jarvis-chat` (always-on daemon, typing, brain replies). Extend it rather than starting another daemon or reply path.
- Within 1s of any owner message: add a 👀 reaction and start the typing indicator. Replace the reaction with ✅ when handled, or ❌ with a short reason on failure. This is never blocked by the model or the database: if the database is down, acknowledge anyway and queue the work in memory.
- Done when: tests check the order of reactions. Live: p95 time from send to reaction is under 1.5s over 20 messages.

## R1.5a — Measure options for fast chat while the GPU is busy

- Measure, with the brain loaded and a build round generating:
    - a small CPU-only model through Ollama (`num_gpu 0`, for example a 4B and an 8B), checking time to first token and tokens per second on this CPU;
    - whether a second Ollama slot for chat fits (`OLLAMA_NUM_PARALLEL=2` at a smaller context; it probably won't with about 1 GB free);
    - a capped API fallback (Haiku-class, through the cost gate).
- Report the numbers and a recommendation to Nathan with `jarvis-discord ask`.
- Done when: the numbers are in `docs/discord-latency.md`, and Nathan has picked an option.

## R1.5b — Front-desk replies

- Do what Nathan picked in R1.5a. Every chat message first gets a quick reply from the fast path, which can see `!status` data. When it needs the brain, it says so ("I'll ask the main model; this may take a few minutes") and queues a `voice` job whose answer is delivered through the outbox.
- Done when: during a live build round, p95 time to the first reply is under 10s over 10 chats, and the brain's answers arrive later in the same thread.

## R1.6 — Talk to the round that is running

- A DM starting with `>` (or `!tell …`) goes into an inbox for the current round (`rounds` row, table `round_inbox`). A Claude Code hook in the agent's settings (check the current hooks docs; for example a PostToolUse hook that returns extra context) passes any unread inbox messages into the running session after each tool call and marks them read. The round's brief tells it to acknowledge them with `jarvis-discord notify`. With no round running, reply "no round is running" and offer `!add`.
- Done when: a test of the hook script with a fake inbox passes. Live: send `> also update the README` during a round, and the agent acknowledges it within one tool call.

## R1.7 — Progress pushes

- Short pushed updates, at most one every 30s with extras batched:
    - round started (job, driver);
    - PR opened (link);
    - job failed (reason);
    - waiting for you (question);
    - round ended (exit code, duration).
- `!verbose on|off|errors` sets the level, stored in the database.
- Done when: tests cover throttling and levels. Live: a whole round produces 3 to 5 messages at the default level.

## R1.8 — Reliability and latency check

- `scripts/discord_reliability.sh` with Nathan in the loop:
    - restart the bot service during traffic;
    - block outbound Discord for 60s;
    - send 20 DMs across both.
- Then check from the database: every message processed exactly once, every reply delivered, and p50 and p95 for acknowledgement, command reply and first chat reply.
- Done when: the results are in the PR, meet the goals at the top of this doc, and anything that missed has a follow-up job queued.
