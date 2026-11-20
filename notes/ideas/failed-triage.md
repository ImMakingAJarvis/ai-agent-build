# failed-triage — 62 failed jobs: 57 stale, 5 worth requeueing

2026-11-19, think job. Sources: `~/.local/share/jarvis/jobs/*/state.json`
(status=failed, `error` field), round output tails, `gh pr list`, the vector
graph, `docs/benchmarks/`. Note only — no jobs moved.

## What the 62 failures actually are

Each job counted once, by the error that killed its round:

| # | Failure mode | n | When |
|---|---|---|---|
| 1 | Photo round ended, no `.sent` delivery marker | 7 | 11-05/06 |
| 2 | Engine down at dispatch (ECONNREFUSED :11434) | 12 | 10-24/25, 11-04/05 incident, 11-12 |
| 3 | Output token max hit (32k ×5, 128k ×4) | 9 | 10-24/25 |
| 4 | llama.cpp Jinja: "System message must be at the beginning" | 8 | 10-24/25 |
| 5 | Autocompact thrashing (context refilled to limit ×3) | 11 | 10-24/25, 11-05 |
| 6 | Prompt too long (128K ctx overflow) | 4 | 10-31..11-18 |
| 7 | Image input not supported (brain has no mmproj) | 6 | 10-31..11-05 |
| 8 | Round killed mid-run (143 ×2, OOM-kill ×1) | 3 | 10-25, 11-04 |
| 9 | git worktree attach failure (exit 128) | 1 | 11-06 |
| 10 | Reviewer API key invalid | 1 | 11-13 |

Two things stand out:

- **~45 of 62 died in two incident windows**: the 10-24/25 build-out era and
  the 2026-11-04 MOE incident (a bench round stopped the brain; 11 rounds
  died on ECONNREFUSED, per `notes/agent/done.md` 11-05). Both root causes
  are since fixed — `jarvis-watchdog` restarts a dead engine (PR #165) and
  `jarvis-jobs next` is driver-aware (skips jobs whose engine is down).
  This whole class should self-heal now.
- **Most of the rest are stale** — the ask already shipped by another path
  (verified against the merged PR list and docs), or the underlying work was
  rebuilt (photo pipeline, MoE bench epic, real app, t2i bench).

## Per-category verdict

**Skip (57).** Verified stale / superseded:

- phase-3b/5/7 build-out jobs (12, modes 2/5) — slot management, front-desk
  model, two-way discord, secrets handling: all exist in the current stack
  (resourced, iGPU helper, jarvis-chat, jarvis-secrets).
- PR-32/33/34 conflict jobs + "fix all merge conflicts" (5, mode 4) — long
  merged; "fix and merge all open PRs #248" (mode 2) — merged.
- q6 asks (2, mode 4) — benched: `docs/benchmarks/qwen38-27b-q6-results.md`
  + q6-vs-q4; brain deliberately stays Q4_K_M. "Keep ctx ≥ 96k" — engine is
  128K now.
- MOE support asks (2) — superseded by the MoE bench epic (PR #262 harness,
  `moe-bench-*` series, D1 Flash-Next-MoE job in the ready queue).
- Kanban site ask — built (task-runner :8080, PR #247). Photo-gen-model ask
  — pipeline exists (art :8120, image.generate worker, EVB-27 window).
  GLADOS voice ×2, chatbot-latency ×3, worker bot token, reasoning-effort,
  ": SCHEDULE", fuser-restart procedure, website-crash — all overtaken.
- C1 run_task loop (mode 6) — shipped 11-06 (PR #173).
- t2i -2 (20-prompt set) — merged (PR #257); t2i -3 (model download + smoke
  test) — done 11-06 (job chat-1794091549-4272 is `done`; only the kanban
  task status never flipped).
- pr76-realapp-scaffold — real app shipped (jarvis-mobile, PR #241).
- GLM sweep -2 (pt1 100MB, killed) — redone, result doc exists; GLM sweep
  -7 (pt6 400MB + comparison, killed) — the missing work is captured by the
  GLM-tail job below.
- review-pr-253 — PR #253 is merged; the reviewer key issue is fixed (PR
  #254 added the local fallback).
- 10 of the 11 photo jobs (modes 1/2/5) — pipeline rebuilt since (window
  controller, `photo` tag, executor with delivery marker).
- MoE cache-plan job chat-1795017563-20e9 (mode 6) — superseded by the
  moe-bench sweep series + the ready D1 job.

**Requeue (5) — still valuable, cause fixable:**

1. `plan-jarvis-build-plan-open-s-1793478730-4` (Reel: library scanner,
   mode 2) — **blocks Reel -5..-16, all 12 sitting in the ready queue.**
2. `moe-bench-200` (mode 6) — MoE vram sweep pt 3/4; 100/150/250 done.
   If "prompt too long" recurs, slim the spec's prompt first.
3. `voice-call-e2e-1` (mode 6) — PR #263 (voice call mode) merged, but the
   E2E never ran; requeue with a slimmer prompt.
4. `plane-run_task-f6eea5c1…` (TST-3: dashboard `/health`, mode 9) — the
   endpoint really is missing: 404 live, no route in `cmd/dashboard`, and
   the earlier "done" job (11-06) only exercised the loop E2E. Fix the
   worktree attach (stale `.worktrees/epic-TST-2` dir) before re-running.
5. One photo job — requeue a single "make a test image" through the current
   image-queue / `photo`-tag path to verify the delivery-marker fix
   end-to-end.

**Fix-first, then requeue (one new job, not one of the 62):** the GLM 5.3
flash sweep is missing points 150MB (GLM-3) and 400MB (GLM-7, the killed
-7) plus the cross-point summary — result docs exist only for
100/200/250/300MB. One small job: run the two points via the `bench.run`
capability (not a brain round) and write
`docs/benchmarks/glm53-flash-vram-sweep-summary.md`.

## Systemic notes

- `state.json` already carries the real failure reason; `jarvis-jobs list
  failed` prints only titles. A `--errors` flag (or a reason column) would
  make this triage a 10-second job next time.
- The task-runner's worktree attach should recreate a missing worktree dir
  instead of exiting 128 — that is what killed the TST-3 attempt.
- The failed pile is an artifact of the pre-watchdog era; nothing in the
  last week's failures indicates a live defect.

**Recommendation:** skip 57, requeue the 5 above, and queue the GLM-tail
bench.run job. Nathan's call via the 08:00 digest.
