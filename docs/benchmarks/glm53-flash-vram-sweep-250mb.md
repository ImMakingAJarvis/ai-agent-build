# GLM 5.3 flash VRAM sweep — point 4/6: ~250 MB free (GLM-5)

Run 2026-11-10 on the RTX 4090 (24,564 MiB; ~22,200 MiB free with desktop
processes resident). Engine: `ft serve` (FreeToken), `UD-IQ2_XXS` (provenance in
`glm53-flash-vram-sweep-100mb.md`), first-shard model path, `ninja` on PATH,
`--moe-strategy hybrid --moe-cache-auto --port 11436`. Brain engine (compose
`jarvis-engine`) stopped for the run, restored afterwards.

## Sizing

`--memory-ratio` calibration (free VRAM after load + CUDA-graph capture):

| r | free after load (MiB) | moe_cache_size | notes |
|---|---|---|---|
| 0.972 | 265, 247, 284 (3 starts) | 1531 | **final r — inside 250 ± 40** |
| 0.971 | 280 | 1528 | inside target |
| 0.9715 | 296 | 1530 | just above target |

Final r = **0.972**, measured free VRAM **247–284 MiB** (engine log "Free GPU
memory after capturing CUDA graphs: 0.25–0.28 GiB"); start-to-start jitter is
~±20 MiB.

## Result: nothing can be served at ~250 MB free

Same failure as the 200 MB point, one step up. Five starts (r = 0.972 ×3,
0.971, 0.9715; free 247–296 MiB), probes with the first 450 chars of the standard
prompt or the full 5,820-char prompt:

| Start | Free after load | Request | Failure |
|---|---|---|---|
| r=0.972 #1 | 265 MiB | standard prompt (~1.5K tok) | passed KDA autotune, then `ggml_cuda_mul_mat_q` OOM: 128 MiB needed, 134 MiB free |
| r=0.972 #2 | 247 MiB | 450 chars | KDA Triton autotune OOM (256 MiB, scratch) |
| r=0.971 | 280 MiB | 450 chars | KDA autotune OOM: 256 MiB needed, 261.8 MiB free |
| r=0.9715 | 296 MiB | 450 chars | KDA autotune OOM: 256 MiB needed, 255.8 MiB free |
| r=0.972 #3 | 284 MiB | 450 chars | KDA autotune OOM: 256 MiB needed, 243.4 MiB free |

The first request itself takes ~25–40 MiB of the free pool before the
`fused_kda_gate_chunk_cumsum` autotuner asks for a 256 MiB scratch, so the
engine needs >~300 MiB free at idle for the autotune to be reliable. Even when
the autotune squeaked through (start #1), the first prefill's matmul scratch
did not fit. The worker dies, the API server hangs in "Waiting for connections
to close" (kill by PID; orphan `spawn_main` workers keep ~21 GB).

**The standard 3-run table cannot be produced at this point, and neither can a
substitute table** (no prompt size completed a request).

## Max context at this cache size (r = 0.972, ~250 MiB free)

**0 tokens served.** The engine starts and answers `/v1/models` but cannot
complete a forward pass.

## Summary

At ~250 MB free (r = 0.972) FreeToken + GLM 5.3 flash (UD-IQ2_XXS, hybrid)
loads and captures CUDA graphs but serves nothing: the first-request KDA Triton
autotune needs 256 MiB scratch on top of ~40 MiB of runtime use, so the floor is
~300 MiB free; 300 MB (GLM-6) is marginal and 400 MB (GLM-7) the realistic first
working point.

## Notes

- Brain restored with `docker compose start engine`: answers on :11434, iGPU helper
  `ok` on :11435.
