# moe-scout — 35B-A3B MoEs are the first class that can beat the 27B brain on every axis

Think job, 2026-11-20. Question: which currently-released open models fit the
4090's 24.5 GB at Q4, can beat the current Qwen3.8-27B dense brain, and which
should be benched next with the existing suite.

## The bar (measured, this box)

Brain = Qwen3.8-27B Q4_K_M (16.8 GB) on llama.cpp, 128K ctx, ~20.4 GB VRAM
(`qwen38-27b-q4-128k-256k.md`): ~48 tok/s decode short prompt, 2,284 tok/s
prefill at a 94K-token prompt, still 37 tok/s decode there. "Beat the brain"
needs ≥ its quality (it is the 3.8 generation), ≥ ~32–64K usable context, and
ideally ≥ 48 tok/s.

## What this machine has already measured

| Model | Setup | Result |
|---|---|---|
| qwen3:30b-a3b (30B/3.3B active, Qwen3 gen) | Ollama, Q4, ~23 GB, 49K ctx | **212 tok/s gen, 162K tok/s cached prompt** — 2× the brain, but an older generation (quality never tested here) |
| Qwen3.8-Flash-Next (~128B, IQ4_XS, FreeToken hybrid) | 250 MB free VRAM | 47–52 tok/s gen, 665 tok/s prefill, **practical ctx ~3–4.5K, hard cap 8,256 tok** |
| GLM-5.3-Flash (320B/18B active, IQ2_XXS, hybrid) | 100–300 MB free | **serves nothing** — 256 MiB Triton autotune floor, OOMs even a 20-char prompt |

Two conclusions from that history: (1) VRAM-resident is where the speed wins
live — the hybrids load fine off 128 GB RAM but their expert cache + KV +
scratch eat the card and context collapses to single-digit-K; (2) the CTX
"128K suite" (plan `moe-128k-suite-plan.md`, harness in `bench/coding/`:
standard prompt, 5 coding exercises / 86 test checks, 128K needle) was planned
2026-11-18 but its GPU phase never ran — the plan job
`chat-1795017563-20e9` is in `failed` and no ladder-point jobs are in the
queue. So whether *any* hybrid can serve 128K is still an open question on
this box.

## Candidates that fit 24.5 GB at Q4 (checked on HF 2026-11-20)

- **Qwen/Qwen3.6-35B-A3B** — official base, 36.0B total / 3B active.
  Q4_K_M ≈ 20.5 GB weights → ~2–4 GB left for KV; with q4_0 KV (the brain's
  own setting) that is roughly **48–96K context** (estimate, verify in the
  bench). Same A3B architecture as the 212 tok/s 30B-A3B measurement, one
  generation newer, and total params above the brain's 27B. This is the
  headline: **the first MoE class where speed, context and quality-generation
  can all clear the bar simultaneously.** Unsloth has UD quants plus an
  `-MTP` variant (multi-token-prediction — check llama.cpp support at pull).
- **LFM2.5-24B-A2B** (Liquid) — 24B/2B active, Q4 ≈ 14 GB → 128K context fits
  with real headroom. Strong coder line; bench as fast-coder / `llm.small`
  upgrade candidate.
- **Tiel-Coder-35B-A3B** (coder-tuned derivative of the 35B-A3B, MTP) — second
  point in the same class, worth the coding battery if the base is close.
- gpt-oss-20b (21B/11.7B active, ~12 GB MXFP4) — fits easily, but 11.7B active
  means ~2× the brain's decode cost per token; fine as a spare, not a winner.
- Out of scope: DeepSeek-V4.1-Flash (**763B** — won't fit 128 GB RAM even at
  IQ2), GLM-5.3-Flash (320B — loads, serves nothing), 128B-class hybrids
  (context-capped, above). No official Qwen3.8-35B-A3B base exists — only
  community distills — so 3.6 is the newest official base in the class.

## What to bench next (existing suite, in order)

1. **Qwen3.6-35B-A3B on llama.cpp** (the brain's engine — not Ollama; the
   30B-A3B reference used Ollama, so same-engine numbers are needed). Pull
   unsloth UD-Q4_K_M (record exact file size), sweep 32/48/64/96K ctx on
   :11436 with `bench/coding/`: standard 3-run prompt + coding battery +
   needle. Compare against the 27B brain at the *same* context points.
   Expect ~150–250 tok/s gen (3B active, same arch as the 212 tok/s point).
2. **Resume the CTX 128K suite** for Flash-Next + GLM — the fail-fast rule
   says run the 400 MB point first; if 128K KV can't be served there the
   hybrid question closes and GPU time stops being spent on 128B/320B models.
3. **LFM2.5-24B-A2B Q4** through the same battery at 128K — the low-VRAM
   option that keeps Whisper/VLM room on the card.
4. **Tiel-Coder-35B-A3B** through the coding battery (only if #1 is in the
   ballp ark of the brain's score).
5. **Close GLM-7 (400 MB point)** to finish that sweep — expected still-OOM
   (floor measured at ~340 MiB), one cheap point to end the project.

## Recommendation

Bench **Qwen3.6-35B-A3B (Q4_K_M, llama.cpp, 48–96K ctx)** first — it is the
only candidate that can beat the 27B brain on quality-generation *and* speed
at acceptable context; keep the 27B dense as brain until a 35B-A3B passes the
coding battery at ≥ brain parity. Treat the 128B/320B hybrids as
planner-class only if the CTX suite proves 128K is servable (current evidence
says no). Open items: no MoE here has had its *quality* measured — tok/s is
the only axis with data so far; the coding battery is the missing piece, and
it already exists in `bench/coding/`.
