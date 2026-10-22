# H — Chat schedules coding work; it does not code

Read only the section for your job. Background, shared by all H jobs:

- Goal (Nathan, Oct 22): Discord chat must talk, capture intent, and schedule coding for the worker. It must not try to write or edit code itself.
- `jarvis-chat` is a tool-less Ollama call (`cmd/jarvis-chat/main.go`). It has no filesystem and no editor. Coding is a Claude Code round from `agent-run` (`jarvis-jobs run`).
- The live coding path is the **agent job store** (`internal/agentjob.Store.Ingest`, `~/.local/share/jarvis/jobs/<id>/` as `agent`). `agent-run` takes the next ready job. RabbitMQ `coding` jobs go to the Go worker as a tool-less generate call — that is the wrong path for "build this". Do not publish coding work there.
- `jarvis-jobs` has no `add` yet (H1.2). `enqueue` is not installed. The store may be empty until J0.12–J0.13; enqueue must still succeed. Consumption needs J0.12, J0.13, and a running `agent-run`. After the BIOS reboot, `agent-run` is **not running**. Do not start it. Say so in the Discord confirm.
- Do not pause or kill a round when scheduling. Only `!pause` interrupts (C1.4, group S). Scheduling is enqueue, not preemption.
- D5.1 later adds `!add` for voice/coding/background. Agent/coding-from-chat uses this group. Extend H, do not build a second enqueue path.

## H1.1 — Seed note and chat prompt: you do not code

- Files: `cmd/jarvis-chat/system_prompt.txt` (the file the daemon embeds and `$JARVIS_CONF/chat-prompt.txt` overrides), `docs/notes/seed/chat-schedules-work.md` (under 80 words, tags `core,chat`), `cmd/jarvis-chat/main.go` `buildMessages` / `systemPromptText` only.
- Note and prompt both say: no shell, no file edits, no systemd, no secrets; talk to Nathan and create jobs for agent-run / jarvis-jobs (`!build` / `!fix` or a `SCHEDULE` block). If `/var/lib/jarvis/notes` exists, copy this seed id only when missing.
- Done when: a test checks the prompt forbids coding and mentions jobs, and the seed file is in the repo. (A Cursor round on Oct 22 wrote the prompt file; re-check it still matches, then tick this job.)

## H1.2 — `jarvis-jobs add`

- File: `cmd/jarvis-jobs/main.go` and its test only.
- `jarvis-jobs add -id ID -title T -driver local|sonnet -source discord -priority N` reads instructions from stdin (or `-instructions`). Calls `Store.Ingest`. Prints the id. Idempotent: a second add of the same id prints `exists` and exits 0.
- Done when: tests against a temp store cover add, duplicate, and `go test ./cmd/jarvis-jobs` passes.

## H2.1 — `!build` and `!fix` enqueue an agent job

- File: `cmd/jarvis-chat/main.go` only (command branch before the brain call) and its test.
- `!build <title and body>` and `!fix <title and body>`: the Go daemon calls `Ingest` (or `jarvis-jobs add`) with `source: discord`, `driver: local`, high priority, id `chat-<unix>-<4 random chars>`. Title is the first line or first 80 characters. Do not call the brain. Do not write `pause-signal`.
- Reply: job id, that it is an agent job for the worker, and either "agent-run is up; it will pick this up at the next checkpoint" or "queued; agent-run is not running, so it will wait" (`pgrep -f '/usr/local/bin/agent-run'` or `$CONF` pid file, no `kill`).
- Done when: tests with a temp store cover enqueue, the confirm text, and no Chat call.

## H2.2 — Model `SCHEDULE` block; Go enqueues it

- File: `cmd/jarvis-chat/main.go` only, plus its test.
- After the brain returns, if the reply contains a block starting with `SCHEDULE:` then `title:` and `instructions:` lines (or a fenced `schedule` block), the Go daemon enqueues as in H2.1, strips that block from the Discord message, and appends the same confirm line. If the block is malformed, say so and do not enqueue. Prompt (H1.1) tells the model to emit this instead of code when Nathan asks to build or fix something and did not use `!build`.
- Done when: tests cover a good block, a malformed block, and a normal chat with no block.

## H2.3 — Scheduling does not pause the round

- File: `cmd/jarvis-chat/main.go` and its test only.
- `!build`, `!fix`, and a `SCHEDULE` enqueue must not create `$CONF/pause-signal`. `!pause` still writes it (C1.4).
- Done when: tests check `pause-signal` is absent after `!build` and present after `!pause`.

## H3.1 — Live check with Nathan

- From Discord: `!build add a tiny /health HTTP handler on the dashboard`. Confirm the id, `jarvis-jobs show <id>` as `agent` has the text, and the confirm says agent-run is not running (unless Nathan started it). Then a normal "how do I enqueue work?" question: the reply must not contain Go code, and may tell him to use `!build`.
- Done when: both messages are in the Discord thread and the job is in the store. Do not start `agent-run`.

## H3.2 — Point D5.1 here; short how-to

- Files: `docs/jobs/d-discord.md` (the D5.1 section only), new `docs/handoff.md` (under 50 lines).
- D5.1: agent jobs from chat already go through H (`Ingest`). `!add agent` should call the same helper. Voice/coding/background may still use RabbitMQ.
- Done when: the pointer is in D5.1 and `docs/handoff.md` names `!build`, `SCHEDULE`, and the store path.
