# Qwen3.8-Flash-Next VRAM sweep — cross-point summary

The cross-point comparison the sweep plan called for once all four points
landed. Point files: `qwen38-flash-next-vram-sweep-{100,150,250}mb.md`
(200 MB never produced a file — see below).

## Setup (common to all points)

- Model: Qwen3.8-Flash-Next MoE (~128B, `UD-IQ4_XS` GGUF, experts
  offloaded to NVMe), engine `ft serve` (FreeToken 0.2.1),
  `--moe-strategy hybrid --moe-cache-auto`, port 11436.
- Card: RTX 4090, 24,564 MiB. `--memory-ratio` sizes the engine so a fixed
  amount of VRAM stays free after load — that free VRAM is what the expert
  cache / KV get (the variable under test).
- Standard prompt: `moe-sweep-prompt.txt` (5,820 chars, 1,496 tokens),
  `max_tokens` 256, temperature 0.7, 3 runs per point.
- Runs: 100/150 MB on 2026-11-04, 250 MB on 2026-11-10.
- Prompt tok/s is a **lower bound** in every table (prompt tokens divided by
  a `max_tokens=1` call, so it includes first-token latency).

## Key metrics per memory configuration

| Free VRAM (measured) | `--memory-ratio` | Standard 1.5K prompt | Prompt tok/s | Gen tok/s | Total | Max context served |
|---|---|---|---|---|---|---|
| ~100 MB (137 MiB) | 0.9825 | **OOM — cannot serve** | — | — | — | ~281 tok |
| ~150 MB (150 MiB) | 0.982 | **OOM — cannot serve** | 105–116 (259-tok prompt) | 44–54 (259-tok prompt) | 7.0–8.2 s (259-tok prompt) | ~259 tok |
| ~200 MB | — | **not run** (job `moe-bench-200` failed 2026-11-10, driver API error; no result file) | — | — | — | — |
| ~250 MB (234 MiB) | 0.9775 | served **only on a warmed server** (cold OOMs) | 663–666 (lower bound) | 47.0 / 52.3 / 51.2 | 7.71 / 7.14 / 7.25 s | ~3–4.5K tok (state-dependent); hard cap 8,256 tok |

Full 3-run tables (150 MB point, 259-token substitute prompt):

| Run | Prompt tokens | Prompt tok/s | Gen tokens | Gen tok/s | Total |
|---|---|---|---|---|---|
| 1 (cold) | 259 | 105.4 | 256 | 44.5 | 8.21 s |
| 2 | 259 | 115.8 | 256 | 53.6 | 7.01 s |
| 3 | 259 | 115.9 | 256 | 53.4 | 7.02 s |

Full 3-run tables (250 MB point, standard 1,496-token prompt, warmed server):

| Run | Prompt tokens | Prompt tok/s | Gen tokens | Gen tok/s | Total |
|---|---|---|---|---|---|
| 1 (warm) | 1,496 | 662.6 | 256 | 47.0 | 7.71 s |
| 2 | 1,496 | 665.7 | 256 | 52.3 | 7.14 s |
| 3 | 1,496 | 666.2 | 256 | 51.2 | 7.25 s |

Orientation run **outside the sweep band** (100 MB point, r = 0.97 → 390 MiB
free): standard prompt served — prompt + first token in 2.4 s (~623 tok/s
lower bound), 256 generated tokens in 7.1 s (~35 tok/s generation). Single
run, non-standard sizing; not a sweep point.

## Findings

1. **The standard 1.5K-token prompt flips from fail to serve somewhere
   between ~190 and ~390 MB of free VRAM.** At 100/150 MB it OOMs the
   scheduler worker on first prefill (worker dies, API shuts down, client
   hangs); at 250 MB it serves only after a warm-up ramp; at 390 MB it
   serves cold.
2. **Generation speed at the best-measured point (~234 MB free) is
   ~47–52 tok/s; prefill of the 1.5K prompt is ~665 tok/s (lower bound).**
   Not comparable to the Ollama qwen3:30b-a3b reference (~212 tok/s gen,
   ~162K tok/s cached prompt): different model (128B vs 30B MoE), different
   engine, and the prompt numbers here include first-token latency.
3. **Context is capped by prefill activation headroom, not KV.** The default
   `--kv-reserve-tokens` gave 8,192 KV tokens in every run; prompts OOM in
   the worker well before that. At 250 MB the practical range is ~3–4.5K
   tokens and it is state-dependent (a warmed server sometimes failed at
   ~3K, sometimes reached 4,501). The absolute hard cap is 8,256 tokens
   (HTTP 400 `context_length_exceeded`).
4. **Free VRAM falls ~25–90 MiB after the first requests** (lazy kernel /
   workspace allocation), so the *effective* spare is below the idle number
   at every point.
5. **The 200 MB point is missing from the sweep.** `moe-bench-200`
   (kanban `ccb3a8f3`) failed 2026-11-10 with a driver-side "Prompt is too
   long" API error before running any benchmark; the kanban task is still in
   backlog. Given the 190/390 MB flip band, 200 MB would most likely OOM on
   the standard prompt like the 150 MB point.

## Open items

- Re-run the 200 MB point (job `moe-bench-200` is in `failed`) to pin down
  the fail→serve flip.
- Nathan's original ask also listed a 300 MB point; no kanban task or job
  exists for it. A 300 MB run would likely serve the standard prompt cold
  and is the natural next point.
- The `ft serve` startup gotchas (first-shard model path, `ninja` on PATH,
  watchdog maintenance window before stopping the brain) are recorded in the
  individual point files.
