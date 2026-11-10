# GLM 5.3 flash VRAM sweep — point 5/6: ~300 MB free (GLM-6)

Run 2026-11-10 on the RTX 4090 (24,564 MiB; ~22,200 MiB free with desktop
processes resident). Engine: `ft serve` (FreeToken), `UD-IQ2_XXS` (provenance in
`glm53-flash-vram-sweep-100mb.md`), first-shard model path, `ninja` on PATH,
`--moe-strategy hybrid --moe-cache-auto --port 11436`. Brain engine (compose
`jarvis-engine`) stopped for the run, restored afterwards.

## Sizing

| r | free after load + CUDA-graph capture (MiB) | moe_cache_size | notes |
|---|---|---|---|
| 0.970 | 344 (log: 0.35 GiB) | 1521 | single start; just above the 300 ± 40 band |

Final r = **0.970**, measured free VRAM **344 MiB**, ~4 MiB above the band. I did
not iterate further: the 250 MB point (r 0.971–0.972, 247–296 MiB free) already
failed at every start, and this start failed with *more* headroom than any of
those (below), so a closer r would not change the outcome.

## Result: nothing can be served at ~300 MB free

First request (first 450 chars of the standard prompt) hung; the worker died with
`torch.OutOfMemoryError: Tried to allocate 256.00 MiB ... 317.31 MiB is free`
(the KDA Triton autotune scratch, same site as the 200/250 MB points), then
"Backend worker is gone and cannot be restarted; stopping the API server".

The 256 MiB allocation fails even with 317 MiB nominally free, so the floor is
above ~340 MiB at idle once CUDA context and allocator fragmentation are counted.
The standard prompt was not attempted (a 450-char probe already fails).

**The standard 3-run table cannot be produced at this point, and neither can a
substitute table.**

## Max context at this cache size (r = 0.970, ~344 MiB free)

**0 tokens served.** The engine starts and loads but cannot complete a forward pass.

## Summary

At ~300 MB free (r = 0.970, 344 MiB measured) FreeToken + GLM 5.3 flash
(UD-IQ2_XXS, hybrid) still dies in the first-request KDA autotune (256 MiB scratch
vs 317 MiB nominally free); the first working point is 400 MB (GLM-7) or higher.

## Notes

- Orphan `spawn_main` worker survives the API server's exit and holds ~21 GB; kill by PID.
- Brain restored with `docker compose start engine`: answers on :11434, iGPU helper
  `ok` on :11435.
