# Qwen3.8 27B Q6_K vs Q4 on the llama.cpp engine

Measured 2026-10-23 on the RTX 4090 (24.5 GB) with `scripts/benchmark_q6.py` against
`/v1/chat/completions`. Both models ran under identical flags:
`--parallel 2 --ctx-size 98304 --kv-unified --cache-type-k q4_0 --cache-type-v q4_0`
(no speculative/MTP decoding in the engine). `cache_prompt` off, `max_tokens` 1024,
temperature 0.6, top_p 0.95, top_k 20. Code-review prompt, 3 runs each.

- **Q6_K**: `/opt/jarvis-engine/models/jarvis-brain.gguf` (21.98 GB, the live brain).
- **Q4**: `/opt/jarvis-engine/models/jarvis-brain.gguf.bak.q4` (16.84 GB, the previous brain).
  Its exact base (3.6 vs 3.8) isn't recorded; treat it as "the previous Q4_K_M brain".

| Metric | Q4 (16.8 GB) | Q6_K (22 GB) |
|---|---|---|
| VRAM used (nvidia-smi, loaded) | 19.6 GB | 22.7 GB |
| Prompt speed (~640 tok) | ~2,000 tok/s | ~873 tok/s |
| Decode speed | **48.1 tok/s** | **17.1 tok/s** |
| Wall time, 1024 tokens | 21.7 s | 60.8 s |
| Tool call (`bash`, OpenAI tools) | PASS | PASS |
| 3 small tasks (arithmetic, code, fact) | 2/3 (code task hit the 1024-token cap while thinking) | 3/3 |

## Findings

- Q6 decodes **2.8x slower** than Q4 (17 vs 48 tok/s), and prompt processing is 2.3x slower.
  The weights are only 1.3x bigger, so memory bandwidth alone doesn't explain it. My guess
  (not verified, the engine runs with `--log-disable`) is that the 22 GB of weights leave
  too little VRAM at 98K context and some layers or buffers land on the CPU. Test this by
  starting the engine without `--log-disable` and reading the offload line, or by trying
  `--ctx-size 65536`.
- Q6 produced complete answers on all small tasks. Q4's one failure is thinking exceeding
  the 1024-token cap, not a wrong answer, so this run shows no quality difference. A real
  quality comparison needs a larger eval, which this does not provide.
- Tool calls are well formed on both.
- Headroom with Q6 is 1.8 GB total on the card (22.7 of 24.5 GB), less once Whisper and
  other GPU users are counted.
- This does not include the earlier Ollama numbers (qwen3.6 Q4 at ~103 tok/s with MTP); the
  engine has no MTP, so the engine numbers are not comparable to those.

## Reproduce

    python3 -I -u scripts/benchmark_q6.py http://localhost:11434 3

Q6 decode of ~17 tok/s makes a 4,096-token thinking answer take about 4 minutes, which is
what the long agent jobs will feel like.
