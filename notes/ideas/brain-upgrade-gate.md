# brain-upgrade-gate — gate criteria + safe swap procedure for replacing the 27B brain

Think job, 2026-11-20. Follow-up to `notes/ideas/moe-scout.md` (same day).
Decision to set up: stay on Qwen3.8-27B (dense, current brain) or move to
Qwen3.6-35B-A3B (MoE, 3B active). What the quality battery must prove, whether
the candidate fits VRAM/context, and the swap + rollback procedure.

## What we know (measured on this box)

Current brain: Qwen3.8-27B Q4_K_M, llama.cpp, :11434, 128K ctx, q4_0 KV.
Measured (`docs/benchmarks/qwen38-27b-q4-128k-256k.md`): 20.4 GB VRAM at 128K
(22.6 at 256K), decode 47.8 tok/s, prefill 2,284 tok/s at a 94K prompt with
decode still 37 tok/s there, needle PASS. ~4 GB headroom; photo work already
runs in brain-out windows, so headroom is not a constraint that bites today.

Candidate: Qwen3.6-35B-A3B, Q4_K_M ≈ 20.5 GB weights. Same A3B architecture as
the one MoE we have measured here (qwen3:30b-a3b, Ollama: **212 tok/s gen,
162K tok/s cached prompt** — 2× the brain), one generation newer, total params
above the brain's 27B. But no *quality* data for any MoE on this box, and the
30B-A3B number is Ollama — a same-engine (llama.cpp) comparison is required.
Nothing has been benched yet: the CTX suite's GPU phase never ran (plan job
failed 2026-11-18, no ladder points exist).

VRAM/context fit (the honest math): 24.5 − 20.5 GB weights ≈ 4 GB for KV +
overhead. q4_0 KV ≈ ~48 KB/token (48 layers × 8 KV heads × 128 dim × K+V) →
**~80–90K tokens maximum; 128K is not possible** on this card without
CPU-offloading KV (kills the speed win) or q2_0 KV (quality risk). Realistic
operating point: **64–96K ctx**. So the swap trades 128K ctx for ~2× decode
plus a newer-generation quality-gen, and possibly MTP draft speedup if this
llama.cpp build supports it (check at pull — moe-scout open item).

Registry drift worth fixing in the same PR: `config/models.yaml` still lists
the brain as `engine: ollama` — stale since the llama.cpp migration.

## What the battery must prove (the gate)

The harness exists: `bench/coding/` — 5 exercises / 86 test checks (graded by
tests, not vibes), standard 3-run prompt sweep, 128K needle at 0/25/50/75/100%,
OpenAI-compatible endpoint. **Missing: a 27B baseline.** The battery was built
for the CTX suite and has never been pointed at the brain. Step 0: run it
against :11434 (the endpoint is a parameter; the brain stays up — costs no
swap window at all).

| Axis | Brain (measured) | Gate for the candidate |
|---|---|---|
| Coding battery | baseline TBD (step 0) | ≥ brain: no exercise the brain passes may fail; more passes better |
| Decode, short prompt | 47.8 tok/s @128K | ≥ 48 (expect 150–250, 3B active) |
| Prefill, ~94K prompt | 2,284 tok/s | within 50% of the brain |
| Needle 0/25/50/75/100% | PASS @128K | PASS at the chosen ctx (sweep 48/64/96K) |
| Tool use / agent | `agent-local check` PASS | PASS under the Qwen3.6 template (reasoning_effort clamp, thinking-off voice path) |
| VRAM | 20.4 GB @128K | ≤ 22.5 GB at chosen ctx (≥ 2 GB free for alongside-GPU users) |

The battery proves the *model*; it does not prove the *agent*. Post-swap soak:
the EVB-34 five-task event-driven eval (≥4/5) plus 1–2 weeks of real jobs
before calling the swap a win.

## Swap procedure (after the gate passes; Q1.3 precedent: ask Nathan first)

The brain cannot test its own replacement while loaded (two ~20 GB models
don't fit), so even the *bench* takes one swap window: scratch llama-server on
:11436 with the brain stopped, per the CTX-suite method. The swap itself runs
as a dedicated igpu-driver job, or by terminal `--force` — never from a brain
round (invariant 1).

1. `jarvis-watchdog maintain 2h "brain model swap"` — failsafe: at expiry the
   brain is re-armed even if everything below died (docs/brain-swap.md).
2. `docker stop jarvis-engine`; confirm VRAM cleared (nvidia-smi ~0.5 GB).
3. `mv /opt/jarvis-engine/models/jarvis-brain.gguf jarvis-brain.gguf.bak.qwen38`
   (same convention as the kept `.bak.q6` / `.bak.q4`); copy the new GGUF into
   place, verify sha256.
4. If ctx changes: edit the engine service in `docker-compose.yml`
   (`--ctx-size`; q4_0 KV stays). Install the Qwen3.6 chat template into
   `/opt/jarvis-engine/templates/` (compose pins the filename — either update
   the path in compose or keep the same name with new content).
5. `docker compose up -d engine` — not `docker restart`, which would keep the
   old command. Wait up to 15 min for `/health` on :11434.
6. Real checks: a completion actually comes back (not just a 200),
   `agent-local check` exit 0 (proves tool use), standard 3-run for the tok/s
   numbers in the PR.
7. PR: `docs/local-brain.md` (model, VRAM, why, rollback),
   `config/models.yaml` (also fix the stale `engine: ollama`), the stable
   facts in the agent CLAUDE.md (model name, VRAM numbers). Discord notify
   with VRAM before/after (Q1.3's done criterion).

**Rollback (≤ ~5 min):** `mv` the `.bak.qwen38` back to `jarvis-brain.gguf`,
revert the compose edit, `docker compose up -d engine`, wait /health,
`agent-local check`. Keep `.bak.qwen38` on disk until Nathan signs off (Q1
kept 3.6 "until Nathan says remove it"). The voice call engine (:11436, same
GGUF path) follows automatically. `jarvis-jobs next` skips jobs while the
brain is down, so a stuck swap strands no doomed round; `jarvis-swap status`
is the one-screen state view.

## Options

1. **Do nothing** — keep 3.8-27B @128K. 48 tok/s is livable, strongest
   context, zero risk. Correct only if the bench shows the 35B-A3B failing
   the quality gate.
2. **Bench the 35B-A3B on the brain's own engine** (moe-scout's
   recommendation): brain baseline (no window) + one igpu-round sweep of
   48/64/96K ctx on :11436 with the existing battery. Cheap: one window
   bracket + ~1–2 h GPU.
3. **Swap to 35B-A3B @64–96K** — only if the gate passes: accept the
   128K→~96K context loss for ~2× decode and newer quality-gen; keep
   `.bak.qwen38` until the soak is clean.
4. **Tiel-Coder-35B-A3B** — coder-tuned sibling; bench only if the base lands
   in the ballparks of the brain's battery score.
5. **LFM2.5-24B-A2B @128K** — ~14 GB, 128K with real headroom; the fallback
   if context turns out to be non-negotiable (also the fast-coder /
   `llm.small` upgrade candidate).

## Recommendation

Bench first (option 2), gate on the table, swap only if it passes (option 3).
The one unknown that matters is *quality* of the MoE class on this box —
speed is already 2× from one measurement, context is a known trade (96K, not
128K), and the procedure + rollback above make the swap itself low-risk and
reversible in minutes. Start with the brain baseline because it is free — it
also yields the first real "is the 27B brain a good coder" number, which we
do not currently have.
