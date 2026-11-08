# Qwen3.8-Flash-Next VRAM sweep — point 2/4: ~150 MB free (job `moe-bench-150`, task `69b984cf`)

Run 2026-11-04 on the RTX 4090 (24,564 MiB; ~21,406 MiB free with desktop
processes resident). Engine: `ft serve` (FreeToken 0.2.1), `UD-IQ4_XS`,
`--moe-strategy hybrid --moe-cache-auto`, port 11436, first-shard model path,
`ninja` on PATH. Brain engine (compose `jarvis-engine`) stopped for the run.

## Sizing

Final `--memory-ratio` **0.982** → **150 MiB free** after load (nvidia-smi; 0.15 GiB
after CUDA-graph capture per engine log; 140 MiB at the end of a later run).
First request drops it by ~25-45 MiB (lazy workspace allocation).

## Result: standard prompt cannot be served at 150 MB free

Same wall as point 1/4 (`phase-moe-bench-100`): the standard prompt
(`moe-sweep-prompt.txt`, 5,820 chars, ~1.5K tokens) OOMs the scheduler worker on
first prefill (`torch.OutOfMemoryError ... Tried to allocate 30.00 MiB ... 108 MiB
free`), the worker exits, the API server shuts down and the client hangs.
**The standard 3-run table cannot be produced at this point.**

## Max context at this cache size (r = 0.982, 150 MiB free)

Largest prompt served: **259 tokens** (first 900 chars of the fixture).
Served: 20 chars (56 tok), 500 (152), 800 (234), 900 (259). Fails (worker OOM):
950 chars (~270 tok), 1000, 1200, 1400, 1700, 2000, full 5,820. KV is not the
limit (`--kv-reserve-tokens` default → 8,192 KV tokens); prefill activation
headroom is.

## Substitute 3-run table (900-char / 259-token prompt, max_tokens 256, temp 0.7)

Fresh server, run 1 = cold. Prompt tok/s = prompt tokens / time of a
`max_tokens=1` call (so it includes first-token latency — a lower bound);
generation tok/s = 256 / (total − that prefill call).

| Run | Prompt tokens | Prompt tok/s | Gen tokens | Gen tok/s | Total |
|---|---|---|---|---|---|
| 1 (cold) | 259 | 105.4 | 256 | 44.5 | 8.21 s |
| 2 | 259 | 115.8 | 256 | 53.6 | 7.01 s |
| 3 | 259 | 115.9 | 256 | 53.4 | 7.02 s |

Not comparable to the Ollama numbers in `docs/moe-benchmark.md` (different
model, much shorter prompt, prompt tok/s lower bound).

## Summary

At ~150 MB free (r = 0.982) FreeToken serves only prompts up to ~260 tokens
(~53 tok/s generation, ~116 tok/s prompt on that size); the standard 1.5K-token
prompt OOMs, so context is capped by headroom, not KV. The viable spare for the
standard prompt is somewhere between ~190 and ~390 MB (see the 100 MB point).

## Notes

- With `ninja` on PATH and first-shard path (see 100 MB point) startup works first try.
- Restore verified: brain answered on :11434, iGPU helper `ok` on :11435,
  systemd `jarvis-engine` inactive.
