# agent-cost-ledger — a week of operation costs ~$5 of electricity and ~$0 of API; the waste is scheduler churn, not tokens

Measured from the job store (`~/.local/share/jarvis/jobs.jsonl` + per-job
`state.json`), the token ledger's Prometheus series (`jarvis_tokens_total`,
7d/30d increases), agent-run's log, and the Anthropic `usage` blocks embedded
in job transcripts. Window: 2026-11-13 → 2026-11-20 (134 agent jobs
completed, 643 failed attempts, 70 unique jobs).

## What a week actually costs

| Item | Last 7d | Source |
|---|---|---|
| Brain tokens (local) | 11.2M new prefill + 4.0M generated + 243M cache reads | ledger [7d] |
| Job-attributed power (GPU) | 38.2 kWh ≈ **$5.0 @ $0.13/kWh** | jobs.jsonl `kwh` |
| Per completed job | 0.267 kWh ≈ **3.5¢** (avg round ~6–15 min) | 35.79 kWh / 134 |
| Whole-box (idle 130 W + work) | ~$22–25/mo steady | power-economics note (this morning) |
| API spend (Sonnet) | **~$0 this week**; ~**$57 all-time** (16 jobs, list prices) | transcript usage sums |

The $57 all-time breaks down as cache writes $28.7 (50%), cache reads $24.3
(43%), output $4.2, new input ~$0. Caveats: per-job `power_cost` is $0
today because `JARVIS_POWER_RATE` is unset; and the job store has **no token
fields at all** — token attribution is engine-global, not per-job.

**Trap found while measuring:** local (jarvis-brain) job transcripts also
carry Anthropic-shaped `usage` blocks (the SDK surfaces llama.cpp counters).
Pricing those 242 local transcripts at list rates gives a phantom **$636
"API spend"** that is actually local compute costing only electricity. Only
the 16 sonnet-driver jobs (reviewer flow + MOE benches) are real API.

## Top waste factors (ranked, with evidence)

1. **Restart/requeue storms — no backoff on infra failures.** 656 rounds
   died "restarted by owner" (agent.log history lines), in two burst
   windows (11-10, 11-19). 633 of the 7d's 643 failed attempts (98%) are one
   job, `chat-1795079297-e149`: claimed 11-19 11:31, killed ~3 s into each
   round by a ~60 s restart cadence, requeued, re-claimed, killed — until
   it finally finished at 22:57. **11 h 26 m of the job's life spent
   thrashing.** Each kill costs ~0.5¢ of power and a session bootstrap
   (est. ~2K new + ~50K cached tokens → roughly 1–2% of the week's prefill
   and cache traffic, order-of-magnitude), but the real losses are
   wall-clock (the job is blocked the whole time) and 600+ noise records in
   the history. The scheduler re-claims infra-killed jobs with zero
   cooldown.
2. **Context thrash.** "Prompt is too long" / "Autocompact is thrashing:
   the context refilled to the limit within 3 turns … 3 times in a row"
   (2 jobs in 7d, several historically, e.g. `chat-1795017563-20e9`,
   `chat-1792728971-b005`). These burn a *full* round of tokens — not 3
   seconds — and then fail.
3. **Cache-read bloat.** 95.5% of local prompt traffic is cache reads
   (243M vs 11.2M in 7d); the API side is the same shape (80.9M reads vs
   281K output ≈ 288:1), and cache (writes+reads) is 93% of the $57.
   Intrinsic to the agentic loop, but it scales with context size: round
   brief, CLAUDE.md, big tool outputs, notes all re-sent every turn.
4. **API jobs that never start.** 38 of 54 sonnet-driver jobs have no
   usage at all (early death — 401 bad API key, e.g. `review-pr-253` on
   11-14). No tokens wasted, but queue slots and review capacity lost;
   the `local` fallback reviewer is the same model as the author.
5. **Photo windows burned with no artifact.** 8 failed photo jobs: "photo
   round ended but no delivery marker" — the swap window + generation ran,
   nothing landed.

Direct power waste from failures is only 6.4% (2.44 of 38.2 kWh) because
failed attempts die fast; the expensive failures are the long ones
(thrash) and the blocked wall-clock (storm).

## Levers

1. **Backoff/cooldown on infra-caused failures** (small agent-run/agentjob
   change): if the failure reason is in {restarted by owner, engine down,
   signal killed} or the job has failed N×/hour, don't re-claim
   immediately — exponential backoff + per-hour cap. Kills class #1 and
   most of the history noise.
2. **Per-round token accounting**: the SDK result event already carries
   usage; record it on `state.json` + `jobs.jsonl`. Then tokens/job and
   $/job are real for both drivers, and the dashboard cost card shows
   something true instead of $0.
3. **Set `JARVIS_POWER_RATE`** (+ the CPU-socket term from the
   power-economics note) — 5 min, no code; turns the kWh already recorded
   into dollars.
4. **Context guard**: fail fast with a clear reason when a round hits
   prompt-too-long / autocompact-thrash early, instead of burning the
   whole round; keep round brief + notes small. Also the direct lever on
   the 95.5% cache-read volume.
5. **Fix the ANTHROPIC_API_KEY path** for sonnet drivers so reviews
   actually run.
6. **Photo delivery-marker gate**: no artifact → fail fast with reason;
   don't let a window burn for nothing.

## Recommendation

Queue **lever 1** first (biggest measured waste, smallest change) and
**lever 2** second (makes every future cost question a lookup, not an
estimate). Do **lever 3** as a 5-minute env change whenever convenient.
4/5/6 are small hygiene items that can ride along with those.
