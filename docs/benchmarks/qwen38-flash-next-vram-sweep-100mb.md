# Qwen3.8-Flash-Next VRAM sweep — point 1/4: ~100 MB free (job `moe-bench-100`, task `066c8a01`)

Run 2026-11-04 on the RTX 4090 (24,564 MiB; ~21,410 MiB free with the desktop
processes resident). Engine: `ft serve` (FreeToken 0.2.1) on
`UD-IQ4_XS`, `--moe-strategy hybrid --moe-cache-auto`, port 11436.

## Result: the engine cannot serve the standard prompt at ~100 MB free

**The 3-run table could not be produced.** At every `--memory-ratio` that leaves
100 ± 40 MB free after load, the first real prefill OOMs inside the engine and
the scheduler worker dies (the API server then shuts down; a client with no
timeout hangs forever).

| `--memory-ratio` | Free VRAM after load (nvidia-smi) | Tiny prompt ("Say hi", 54 tok) | Standard prompt (~1,496 tok) |
|---|---|---|---|
| 0.99   | ~85 MiB (before CUDA-graph capture; 0.09 GiB per engine log) | — | CUBLAS_STATUS_ALLOC_FAILED during CUDA-graph capture — never serves |
| 0.985  | 90 MiB  | — | OOM on first request (tried 20 MiB, 87 MiB "free") |
| 0.983  | 128 MiB | — | OOM on first request (tried 30 MiB, 85 MiB free) |
| **0.9825** (final) | **137 MiB** (107 MiB after the first request) | OK | **OOM** (`kernel/triton/hc.py grouped_gemma_rmsnorm`, tried 30 MiB) |
| 0.98   | 190 MiB | OK (327-tok prompt) | OOM / hang |
| 0.97   | 390 MiB | OK | **OK, 1,496 prompt tokens** |

Final r for this point: **0.9825**, free VRAM **137 MiB** at idle after load (top
of the 100 ± 40 band; going lower only crashes sooner). Free VRAM falls ~30 MiB
on the first request (lazy kernel/workspace allocation), so the *effective*
spare at ~100 MB is ~100 MiB.

## Max context at this cache size

Largest prompt served at r = 0.9825 (137 MiB free): **281 tokens** (1,000 chars
of the fixture; 153 tok also OK). 1,200 chars (~330 tok), 1,400, 1,700, 2,000
chars (~560 tok) and the full 5,820-char prompt all OOM the worker. So the
threshold lies between 281 and ~330 tokens. At 190 MiB free (r = 0.98) a 327-token prompt
is served, the full prompt is not. The `--kv-reserve-tokens` default allocated
only 8,192 KV tokens (0.19 GiB) in all runs, so context here is limited by
prefill activation headroom, not KV.

## Indicative numbers at 0.97 (390 MiB free) — NOT this sweep point

Single non-standard run for orientation only (standard prompt, 1,496 prompt
tokens, `max_tokens` 256, temperature 0.7): prompt+first token 2.4 s total,
256 generated tokens in 7.1 s → roughly 35 tok/s generation. Not comparable
to the Ollama numbers in `docs/moe-benchmark.md` (different model, one run).

## Summary

At ~100 MB free the FreeToken engine serves only ~280-token prompts, and the
standard 1.5K-token benchmark prompt needs somewhere between ~190 and ~390 MB
of spare VRAM; the 150/200/250 MB points will likely hit the same wall (200/250
MB probably fail on the standard prompt) — their jobs should lower the prompt
or report the failure the same way.

## Notes for sibling jobs

- Model path must be the **first shard** (`.../UD-IQ4_XS-00001-of-00003.gguf`);
  the folder fails ("no `model_type` in config.json").
- `ninja` must be on PATH: `PATH=/home/agent/ftvenv/bin:$PATH ft serve ...`
  (CUDA-graph/triton JIT fails with `FileNotFoundError: ninja` otherwise).
- The HTTP port answers `/v1/models` ~1 minute before the model finishes
  loading; wait for "API server is ready to serve" in the log.
- Always give the client a timeout (and `max_tokens`): when the worker OOMs the
  request never returns.
- The legacy systemd `jarvis-engine.service` (Restart=always) came back by
  itself seconds after `docker compose stop engine` and took the GPU; check
  `nvidia-smi` and `sudo systemctl stop jarvis-engine` before starting `ft serve`.
- Do not `pkill -f "ft serve"` — it matches the Claude CLI; kill the recorded PID.
- Restore: `docker compose start engine` — brain answered on :11434, iGPU
  helper healthy on :11435 after this round.
