# queue-sequencing — the 81 "ready" jobs are 4 deadlocked chains; unblock the heads, then let the queue run

## What I found

`jarvis-jobs list ready` shows 83 jobs (81 real + 2 think), but `jarvis-jobs next`
returns only a think job. The queue is not waiting on sequencing — **every real
chain is gated by a failed or missing dependency**, and `Store.Next` only hands
out jobs whose `depends_on` are all `done` (internal/agentjob/store.go:500).

The 81 jobs form exactly four chains, each blocked at its head:

| Chain (band) | Jobs | Blocked by |
|---|---|---|
| J1→D0→E1→E2→bot→Q1 (background) | 59 | `j1` depends on `j0-2` — **not in the store at all** (only `j0`/J0.9 exists). Phantom dep from a store migration; can never resolve. |
| Planner epic C2→C3→C4→D1→D2 (build) | 5 | `C2` depends on `C1: run_task execution loop` — **failed** |
| Reel −5→−16 (build) | 12 | `−5` depends on `−4` library scanner — **failed** |
| t2i −4→−8 (build) | 5 | `−4` depends on `−2` prompt set + `−3` model download — both **failed** |

So the machine has been burning think jobs because nothing real is claimable,
not because the queue drained. Everything else (order, bands) is already
encoded correctly in the store:

- Band order is fix→build→background (default, no band_order.json), so the 22
  build jobs (planner, Reel, t2i) fully consume **before** the 59-job
  background mega-chain. Within a band, priority desc.
- The dep edges already encode every real dependency: E1 needs J1's
  `job_events` table, E2 needs E1's data, the bot daemon (d1) needs E2.7, the
  R1 reliability series is interleaved between bot D2 and D3, Reel fans out
  after Gate 2 (−11 Android, −13 Apple, −15 Tailscale all depend on −10;
  field test −16 joins −12/−14/−15).

Conflicts flagged:

1. **Shared `phase-reel` branch** (standing rule in every Reel job): one PR per
   job, ~500 lines, strict −5→−16 order, rebase onto main each round. The dep
   DAG enforces the order, but Gate 2 (−10) is a **decision, not code**: if
   Nathan picks "fork Jellyfin", −11…−16 become waste. Nothing stops the loop
   from continuing past the decision — the tail must be parked on the answer.
2. **"Mid-flight brain switch" is already done.** Q1 (3 jobs, tail of the
   mega-chain) was written for the old **ollama** brain; the brain is already
   Qwen3.8-27B Q4_K_M in the llama.cpp `jarvis-engine` container (snapshot,
   docs/local-brain.md, GGUF + `.bak.q4` = 3.6 rollback copy all present) and
   `ollama list` errors — the server is dead. Running q1→q1-3 as written would
   waste ~3 rounds on a dead runtime. Close them; the live brain question is
   the ready `think-brain-upgrade-gate` job.
3. **VRAM windows.** Two window jobs: t2i −5 (full run; `image.generate` =
   needs brain out) and planner D1 (pull 61 GB Flash Next MoE + benchmark,
   which per the epic spec requires unloading the brain). The window
   controller only brackets **photo-tagged** jobs — D1 as specced doesn't fit
   the mechanism; it needs a tag or a re-spec (e.g. submit as `bench.run`).
   Batch both window jobs in one window epoch to pay the ~2 min bracket once
   per job at most.
4. **Stale premises in three jobs:** D0.3 (systemd dashboard/worker — both are
   compose containers now) and d1-2 (`jarvis-bot.service` — same issue); D0.1/
   D0.2 cite Oct-21 code state and should be re-verified against current main
   before building.
5. **External round-trips:** Reel −5 needs a TMDB API key from Nathan (asked
   in-round if not in env); Reel −13 needs a macOS CI runner for Xcode;
   planner D1's 61 GB pull needs disk headroom check (241 GiB free — fine).
6. **Naming collision:** planner epic's "D1/D2" (MoE bench, model docs) vs
   Discord bot's D1–D8 — easy to mix up in queue listings and Plane comments.
7. **Self-modifying job:** J1.4 points the worker + `jarvis-jobs` at Postgres
   — it replaces the store feeding this very loop. Do it in a dedicated round
   with a rollback plan (keep `jobs.jsonl` dual-written until verified).

## Options

- **A. Unblock in place (minimal).** Requeue the three failed heads (C1,
  Reel −4, t2i −2+−3) and resolve the `j0-2` phantom (finish a placeholder
  `j0-2` job done, or drop the dep). The encoded order then runs itself:
  build band (planner, Reel, t2i interleaved by priority) → background
  mega-chain → think floor.
- **B. Unblock + re-band.** Same as A, plus move the background band first so
  the J1/D0/E1/E2 infrastructure lands before Reel/t2i (dashboard + DB before
  the media app).
- **C. Triage with Nathan first.** Some chains are weeks old; ask which of
  Reel / t2i / planner-epic he still wants before unblocking anything, close
  the rest, and only unblock the survivors.
- **D. Fix the mechanism.** Add a small `jarvis-jobs setdeps`/`undep` command
  so a failed/missing dep can be resolved cleanly instead of a placeholder
  job; then do A or C.

## Recommendation

A, with three guards — it is the smallest change and the queue's own encoding
already expresses the right order:

1. One cleanup round: requeue C1, Reel −4, t2i −2/−3; resolve `j0-2`
   (placeholder done-job is the minimal, reversible fix; D is a nice-to-have
   follow-up, not a prerequisite).
2. Close Q1.1–Q1.3 as stale (evidence above) instead of letting three rounds
   die on ollama; same for D0.3 (or rescope to "verify exactly one worker").
3. Hard-stop Reel at Gate 2 (−10) for Nathan's decision and park −11…−16
   until answered; get the TMDB key before −5.
4. Tag or re-spec planner D1 for the swap window and schedule it back-to-back
   with t2i −5 so the two window jobs share one window epoch.
5. Run J1.4 alone, with dual-write rollback, before anything downstream of
   J1.5 trusts the DB.

Which epics wait: Reel −11…−16 (Gate 2 answer), t2i tail (asset rework +
window), everything after J1.5 (encoded), bot D6/D7 (bot daemon + R1.5a
numbers, encoded), Q1 (never — stale).
