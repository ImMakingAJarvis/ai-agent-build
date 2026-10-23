# Qwen3.8 27B Q6_K vs Q4 at 64,000 context

Measured 2026-10-23 on the RTX 4090 (24.5 GB) with `scripts/benchmark_q6.py` against a
scratch `llama-server` on port 18080 (the `jarvis-engine` service was stopped during the run
and restarted afterwards). Both models ran under identical flags:
`--parallel 2 --ctx-size 64000 --kv-unified --cache-type-k q4_0 --cache-type-v q4_0 -fa on`.
Same sampling as the 98K run: `cache_prompt` off, `max_tokens` 1024, temp 0.6, top_p 0.95, top_k 20.
Code-review prompt (657 tokens), 3 runs each.

- **Q6_K**: `/opt/jarvis-engine/models/jarvis-brain.gguf` (22.0 GB, the live brain).
- **Q4**: the `qwen3.8:27b` Ollama blob (`sha256-f5f1dd89...`, 16.8 GB, GGUF name "Qwen3.8 27B 0814").
  This is confirmed 3.8, unlike `jarvis-brain.gguf.bak.q4`, whose base is unrecorded.

| Metric | Q4 (16.8 GB) | Q6_K (22 GB) |
|---|---|---|
| VRAM used (nvidia-smi, loaded) | 18.9 GB | 22.6 GB |
| Prompt speed (657 tok) | ~1,700 tok/s | ~1,050 tok/s |
| Decode speed | **48.2 tok/s** | **22.2 tok/s** |
| Wall time, 1024 tokens | 21.8 s | 46.9 s |
| Tool call (`bash`) | PASS | PASS |
| 3 small tasks | 2/3 (capital-of-Australia check failed) | 3/3 |

## Findings

- At 64K, Q6 decodes **2.2x slower** than Q4 (22 vs 48 tok/s) and prompt processing is 1.6x slower.
  Cutting context from 98K to 64K lifted Q6 from 17 to 22 tok/s (+30%) and left Q4 unchanged,
  which supports the earlier guess that Q6 at 98K was short of VRAM. It does not explain the
  whole gap: weights are only 1.3x bigger, yet Q6 is still 2.2x slower. The cause is unverified;
  this llama-server build prints almost no offload/buffer lines, so I could not read the layer split.
- VRAM headroom at 64K: Q6 leaves 1.9 GB, Q4 leaves 5.7 GB (for Whisper and other GPU users).
- Quality: one Q4 miss out of 3 small tasks. I did not inspect why (it returned in 123 tokens,
  so not the token cap). Three runs of tiny tasks is not a quality eval; treat it as a flag only.
- Tool calls are well formed on both.
- The service still runs `--ctx-size 98304`; nothing was changed in `systemd/`.

## Reproduce

    python3 -I -u scripts/benchmark_q6.py http://localhost:18080 3
