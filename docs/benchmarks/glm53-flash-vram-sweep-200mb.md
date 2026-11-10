# GLM 5.3 flash VRAM sweep — point 3/6: ~200 MB free (GLM-4)

Run 2026-11-10 on the RTX 4090 (24,564 MiB; ~22,200 MiB free with desktop
processes resident, "Free memory before loading model: 21.30 GiB" in the engine
log). Engine: `ft serve` (FreeToken), `UD-IQ2_XXS` (provenance in
`glm53-flash-vram-sweep-100mb.md`), first-shard model path, `ninja` on PATH,
`--moe-strategy hybrid --moe-cache-auto --port 11436`. Brain engine (compose
`jarvis-engine`) stopped for the run, restored afterwards.

## Sizing

Final `--memory-ratio` **0.975** → **~200 MiB free** after load and CUDA-graph
capture (nvidia-smi: 203, 187, 244, 223, 196, 204 MiB over six fresh starts; engine
log "Free GPU memory after capturing CUDA graphs: 0.21 GiB"). `--moe-cache-auto`
resolved `moe_cache_size=1539` (3 pools, 8,640-token / 0.10 GiB KV).

Ratio calibration (free VRAM after load): r=0.95 → 760 MiB (cache 1463);
r=0.975 → ~200 MiB (cache 1539); r=0.985 → the engine does not even start
(OOM allocating the 3.32 GiB GatedDeltaNet/KDA state pool after the expert pool was
sized). Starting at r≈0.985 as the job text suggests is too high for this model.

## Result: nothing can be served at 200 MB free

The first inference request kills the scheduler worker, **even a 20-character
prompt**. The failure is not prefill activation headroom (as at Qwen 100/150 MB) but
the first-request Triton **autotune** of the KDA kernel:

```
chunk_kda_with_fused_gate -> fused_kda_gate_chunk_cumsum (kernel/fla/kda.py:984)
  triton/runtime/autotuner.py _bench
torch.OutOfMemoryError: Tried to allocate 256.00 MiB ... 202.00 MiB is free
```

The autotuner benchmark needs a 256 MiB scratch allocation, which cannot fit in
~200 MiB. The worker exits, the API server shuts down ("Backend worker is gone")
but the process lingers in "Waiting for connections to close", so clients hang
until their own timeout (and orphan `spawn_main` workers can keep ~21 GB of VRAM —
kill them by PID, not `pkill -f`).

Probes, each on a fresh server (r=0.975, free 187–244 MiB), `max_tokens=1`, 90 s
timeout, prefix of `moe-sweep-prompt.txt`: 20 chars, 450, 900, 1,500, 3,000 and the
full 5,820-char standard prompt (3-run script) — **all fail** identically.

| Prompt | Prompt tokens | Result |
|---|---|---|
| 20 chars | tiny | worker OOM (autotune, 256 MiB) |
| 450 / 900 / 1,500 / 3,000 chars | — | worker OOM |
| standard prompt (5,820 chars) | ~1.5K | worker OOM (first prefill; mhc kernel 12 MiB with 96 MiB free) |

**The standard 3-run table cannot be produced at this point, and neither can a
substitute table** (no prompt size is served).

## Max context at this cache size (r = 0.975, ~200 MiB free)

**0 tokens served** — no request completes. Context window metric for this point:
none (engine starts, answers `/v1/models`, cannot run a forward pass).

## Summary

At ~200 MB free (r = 0.975) FreeToken + GLM 5.3 flash (UD-IQ2_XXS, hybrid) loads
and captures CUDA graphs but cannot serve any request: the first-request KDA
Triton autotune needs 256 MiB scratch, so the usable floor for this model is
somewhere above ~260 MiB free (the 250 MB point is likely marginal; 300/400 MB are
the realistic first working points). The prefill activation limits seen with the
Qwen model are not even reached.

## Notes

- Orphans from the failed 100 MB job (two `ft serve` + `spawn_main` workers, PPID 1,
  from the 11:22 run) were found at job start and killed by PID before this run.
- Maintenance window (brain restarts skipped until 06:24) was active; the brain was
  restored manually with `docker compose start engine`: answers on :11434, iGPU
  helper `ok` on :11435.
