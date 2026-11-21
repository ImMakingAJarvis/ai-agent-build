# discord-bot-minimum — v1 is ~80% already live; ship reliability + visibility, retire the stale 32-job plan

**Bottom line:** the D1–D8 + R series (~31 bot jobs, plus 3 D0 dashboard jobs)
was planned in October when the bot was a webhook. Since then `jarvis-chat`
grew into the daemon the plan wanted to build, so most of the "useful"
surface already ships. The minimum useful v1 is **~7 jobs, not 31**: the
reliability core (never lose a message, tell me the bot is dead, results
always come back) plus four visibility commands. Everything else defers or
retires.

## What I found

1. **The live surface is bigger than the plan.** `cmd/jarvis-chat` (11+
   commands verified in code and in the 2026-11-05 frame doc) already does:
   owner-only DM intake, brain chat with status line + history + reply-to
   context, 👀/typing acks, busy/timeout notices, daily logs, checkpoint
   questions with buttons, reply-to-worker → agent inbox (PR #198), mode
   flags (`!automode`/`!fullauto`/`!keepgoing`), the photo suite
   (`!photo*`), job intake (`!build`/`!fix`), `!igpu`, `!review`, truthful
   in-container status (PR #256), and the Hermes multi-bot runner.
   D1 (daemon+intake), D2 (ask routing — works today via per-call gateway
   sessions), D6 (chat→brain), and most of R1.4 (ack) are done or working.

2. **Usage logs show exactly what's failing** (discord-11-15…11-20):
   - **"add a health endpoint" × 37 in five days.** The request did land —
     as Plane task TST-3 — but the job *failed on an infra bug* (worktree
     already exists) and **the failure never came back to Discord**. He kept
     asking because he could not tell that it had been received, failed, or
     was done. This one pattern justifies the whole reliability layer.
   - **"Are you alive?" / "You alive now?" / "?"** — no way to know the bot
     is dead (a "chatbot has died" job exists). → proactive down-alert.
   - **"What are you working on?" / "what did you schedu;e" / "What are
     the results" / "All the work on plane is done?"** — visibility only via
     brain round-trips or the :8080 dashboard. → instant commands.
   - **"what are the commands I can do"** — no `!help`.
   - Typos in the wild (`schedu;e`) — the parser already tolerates them
     (`!fullautox` in tests); keep that.

3. **The plan is stale in ways that cost queue space:**
   - D1.1 says "build `cmd/jarvis-bot`" but its own note says "extend
     `jarvis-chat`, don't start another daemon". The spec contradicts itself;
     the daemon already exists. D1.2 wants a systemd unit, but the stack is
     compose-only (the `jarvis-chat` container).
   - R1.5a's recommendation (llama3.2:3b on CPU) predates the iGPU helper
     (Qwen2.5-3B on Vulkan, zero CPU cost, deployed 11-11) and the Ollama →
     llama.cpp migration. The front-desk question (R1.5b, skipped) is
     mostly moot: an ack needs no model at all.
   - **R1.1 (gap recovery) has no job in the store** — `show r1-1` is
     "no such file". The one reliability spec with no job is the one that
     guarantees "Nathan never loses a message". It needs re-queuing.

## Options / next steps

- **A. Ship the minimum v1 (recommended).** Seven jobs, all extensions of
  `jarvis-chat` — no new daemon:
  1. R1.3 watchdog + REST-only "bot down"/recovered alert (systemd timer +
     existing REST notify; no gateway needed — the cheapest high-value item).
  2. R1.1 gap recovery: persist last processed DM id, replay on reconnect,
     dedupe. (Re-queue — currently missing from the store.)
  3. R1.2 outbox with retry/nonces + **every accepted owner message gets a
     "queued as <id>" / "failed: <why>" receipt within ~5s**. This is the
     37× fix.
  4. D4.2 `!status` + D4.3 `!queue`/`!prs`/`!failures`, backed by the same
     in-container status path PR #256 built (truthful by construction,
     ~2s, no brain).
  5. D3.1 `!help` from the existing parser.
  6. D5.2 results-to-Discord for agent jobs (the failure case included —
     TST-3's failure should have DM'd him).
  7. D8.1 docs+smoke folded into the v1 PR; D8.2 live checklist once.
  Optional second wave: D5.3/D5.4 `!cancel`/`!move` (queue surgery from the
  phone), R1.7 progress pushes.
- **B. Do the whole plan in order.** Rejected: 31 rounds to reach the same
  v1, ~half of them rebuilding what exists (D1/D6/R1.4), and a second
  daemon the plan itself forbids.
- **C. Retire now, ship nothing:** mark D1.1/D1.2/D1.3, D2.1/D2.2 (measure
  first — only if `ask`'s gateway session ever fights the chat connection),
  D6.1/D6.2, D7.1–D7.3 (10-turn window is fine until conversations outgrow
  it), R1.5b, R1.6 (superseded by PR #198 + the owner-message hook; the
  generic `>` hook is re-evaluable later), R1.7 (until v1 reliability is
  proven) as superseded, with a one-line "covered by <job/PR>" note in
  docs/jobs/d-discord.md so the queue doesn't rot into a trap.

## What makes the bot pay for itself

A 7-day bar, all checkable from the existing logs/database:
1. **Zero "are you alive?" messages** — the down-alert fires first instead.
2. **Zero repeated requests** — every accepted message gets a receipt; the
   "health endpoint ×37" pattern cannot recur.
3. **"What are you working on?" answered by `!status` in <2s**, not a brain
   round-trip or a dashboard tab.

If those hold, the second wave (cancel/move, progress pushes, `>` hook) is
earned. If they don't, the next thing to build is whatever failed.
