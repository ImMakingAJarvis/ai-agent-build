# C — Talk to Jarvis on Discord now, while it works

Read only the section for your job. Background, shared by all C jobs:

- Goal (Nathan, Oct 21): when he messages on Discord, he gets a reply right away, or at least within a conversational delay, even while a build round is running. No engine switch is needed for this. That's group S, later.
- Today the bot only hears him at checkpoints (`jarvis-discord ask`). During a round nobody is listening. The PR #7 watcher in `scripts/agent-run` (`start_watcher`, `pause-signal`) is not installed yet. When it is, it kills the round on every message, and the job starts over.
- How it works: an always-on daemon gets each DM, shows typing at once, and makes one `/api/chat` call to the brain. Ollama runs one request at a time, so the chat waits only for the round's current request to finish, not the whole round. The round is never paused or killed.
- Two Ollama traps. (1) Send only `model`, `messages`, `stream: false` and `think: false`. Different `options` (for example another `num_ctx`) make Ollama reload the 17 GB model. (2) The chat call evicts the round's cached prompt, so the round's next step re-reads it. That costs some seconds, which is acceptable.
- Code to reuse: `internal/chat/discord.go` (`NewDiscord`, `Listen`, `Notify`, owner-only DM filter), `cmd/jarvis-discord/main.go` (`loadEnvFile`, config), and `local.env` (`OLLAMA_URL`, `LOCAL_MODEL`). Read these with `grep -n` and line ranges.

## C1.1 — `jarvis-chat` daemon

- Files: new `cmd/jarvis-chat/main.go` and its test. Add a `Typing` method and an `OnMessage(func)` hook to `internal/chat/discord.go`, and nothing else in that file.
- On each owner DM: within 1 s, add a 👀 reaction and start typing, refreshed every 8 s. Call the brain with a short system prompt ("You are Jarvis, Nathan's home AI. Answer briefly and conversationally."), a status line, and the last 10 turns kept in memory, at most about 3,000 words. The status line is the current job from `$CONF/current-job` plus the last 3 lines of `~/.local/state/jarvis/agent.log`. Reply with `Notify`.
- If there's no answer after 20 s, send once: "Busy on <job>, my answer is coming". Give up after 4 minutes with "Sorry, the brain is tied up; ask me again in a bit".
- Skip a message while a checkpoint question is open (C1.2 adds the marker), and skip messages that are Discord replies to a bot question. Those belong to `ask`.
- Done when: tests with a fake Ollama server and a fake Discord sender cover the reply, the busy notice and the timeout, and `go test ./...` passes.

## C1.2 — Checkpoint answers don't get a chat reply

- File: `cmd/jarvis-discord/main.go` only.
- While `ask` waits, it writes `$CONF/ask-open` and removes it on exit, including on error or signal. `jarvis-chat` skips messages while that file exists, so a typed checkpoint answer still goes only to the round.
- Done when: a test checks the file exists during `Ask` and is gone afterwards.

## C1.3 — Install and run it as a service

- Files: new `systemd/jarvis-chat.service` (`User=agent`, `Restart=always`, environment from `~/.config/jarvis/agent.env`), new `scripts/install_chat.sh`.
- The script builds `jarvis-chat` and `jarvis-discord` from the current checkout, and installs both atomically: copy to a temp name in `/usr/local/bin`, then `mv`. A running `jarvis-discord ask` keeps its old binary, which is fine. Then it enables and starts the unit with `sudo systemctl`.
- Done when: `systemctl is-active jarvis-chat` says active, and a test DM gets a reply. Tell Nathan on Discord that he can message any time now.

## C1.4 — Messages no longer kill the round

- File: `scripts/agent-run` only (`start_watcher` and the paused branch).
- When `systemctl is-active --quiet jarvis-chat` succeeds, don't start the pause watcher, because `jarvis-chat` answers instead. Keep pausing only for a message that is exactly `!pause`. Install atomically, as in C1.3.
- Done when: `bash -n` passes, and a dry run with `AGENT_RUN_FAKE=1` and the service active doesn't pause on a normal message.

## C1.5 — Latency check with Nathan

- File: new `docs/discord-latency.md`.
- During a local round, Nathan sends 5 messages a few minutes apart. Log the time to 👀, to the first reply, and how long the round's next step was delayed (from `agent.log` timestamps).
- Done when: the table is in the doc, and Nathan said on Discord whether it feels conversational. If not, note what he wants. S1.5, S5.1 and R1.5a are the follow-ups.
