# Qwen3.8-Flash-Next VRAM sweep — point 4/4: ~250 MB free (job `moe-bench-250`, task `5669329e`)

Run 2026-11-10 on the RTX 4090 (24,564 MiB; ~22,170 MiB free with desktop
processes resident). Engine: `ft serve` (FreeToken 0.2.1), `UD-IQ4_XS`,
`--moe-strategy hybrid --moe-cache-auto`, port 11436, first-shard model path,
`ninja` on PATH. Brain engine (compose `jarvis-engine`) stopped for the run.

## Sizing

| `--memory-ratio` | Free VRAM after load (nvidia-smi) |
|---|---|
| 0.977 | 291 MiB (just outside the band) |
| **0.9775** (final) | **234 MiB** (225–253 MiB across restarts; ~140 MiB after serving requests) |

Final r = **0.9775**, free VRAM **~234 MiB** at idle after load (target 250 ± 40).
Free VRAM drops ~90 MiB once requests have run (lazy kernel/workspace allocation).

## Result: cold start cannot serve the standard prompt; a warmed server can

- **Cold server, standard prompt (5,820 chars, 1,496 tokens) as the first request:
  OOMs the scheduler worker** (`torch.OutOfMemoryError ... Tried to allocate 20 MiB ...
  ~100 MiB free`), the API server shuts down and the client hangs. Reproduced twice
  (fresh server each time).
- After the server has served a ramp of smaller prompts first (281 → 524 → 761 →
  1,010 tokens), the same standard prompt is served. So the 3-run table below is
  **warm**: run 1 is *not* a true cold-load run (a true cold run does not complete).

## 3-run table (standard prompt, warmed server, max_tokens 256, temp 0.7)

Prompt tok/s = prompt tokens / time of a `max_tokens=1` call (includes first-token
latency, so a lower bound); generation tok/s = 256 / (total − that prefill call).

| Run | Prompt tokens | Prompt tok/s | Gen tokens | Gen tok/s | Total |
|---|---|---|---|---|---|
| 1 (warm) | 1,496 | 662.6 | 256 | 47.0 | 7.71 s |
| 2 | 1,496 | 665.7 | 256 | 52.3 | 7.14 s |
| 3 | 1,496 | 666.2 | 256 | 51.2 | 7.25 s |

## Max context at this cache size (r = 0.9775, ~234 MiB free)

- Largest prompt served: **4,501 tokens** (18,000 chars of the fixture repeated),
  on a server first warmed with a ramp of smaller prompts.
- 22,000 chars (~5,500 tok) and 28,000 chars: worker OOM (client timeout).
- Hard cap regardless of VRAM: the server rejects prompts over **8,256 tokens**
  (prompt + generation; default `--kv-reserve-tokens` KV budget) with HTTP 400
  `context_length_exceeded` (36,000 chars = 8,974 tok).
- The limit is state-dependent: an earlier warmed server failed at 2× fixture
  (~2,990 tok), a later one reached 4,501, and one post-failure probe sequence failed
  at 4,000 tokens. Treat ~3–4.5K as the practical range at this cache size; the
  headroom is prefill activation memory, not KV.

## Summary

At ~234 MB free (r = 0.9775) FreeToken generates at ~47–52 tok/s and prefills the
1.5K-token standard prompt at ~665 tok/s (lower bound) — but only once warmed;
a cold server OOMs on that prompt, and the max served context is ~4.5K tokens.
Spare VRAM between ~190 and ~390 MB is where the standard prompt flips from
fail to serve (see the 100/150 MB points).

## Notes

- The **watchdog restarts the brain** within ~30 s of `docker compose stop engine`
  (restart policy + watchdog), which then takes the GPU and OOMs `ft serve`. Run
  `jarvis-watchdog maintain 2h "<reason>"` first and `maintain off` afterwards.
- Do not `pkill -f "ft serve"` (matches other processes); kill the recorded PID.
- Restore verified: brain answered on :11434, iGPU helper `ok` on :11435,
  systemd `jarvis-engine` inactive, watchdog maintenance off.
