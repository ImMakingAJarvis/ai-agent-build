# Discord Chat Latency Measurements

Measured 2026-10-22 while jarvis-brain was loaded on the GPU (RTX 4090, ~22 GB used, ~631 MB free).
All Ollama benchmarks used `num_gpu: 0` (CPU-only) so the GPU stays free for build work.

## Test Environment

| Component | Spec |
|---|---|
| CPU | Intel Xeon, 16 cores |
| RAM | 122 GB available |
| GPU | RTX 4090 24 GB (~22 GB used by brain model) |
| Ollama | Running locally, CPU-only mode |

---

## Option A: CPU-only Ollama models

### Generate API (no system prompt, 128-token max)

| Model | TTFT (avg, warm) | Tokens/sec | Total for ~100 tokens |
|---|---|---|---|
| llama3.2:1b | 0.057 s | 35.3 tok/s | ~3.5 s |
| llama3.2:3b | 0.089 s | 21.7 tok/s | ~2.3 s |
| qwen2.5:7b | 0.102 s* | 10.5 tok/s | ~5.0 s |

*First run: 6.6 s (cold load). Subsequent runs: 0.10 s.

### Chat API (with system prompt, realistic Discord use)

| Model | TTFT (avg) | Tokens/sec | Avg tokens | Total |
|---|---|---|---|---|
| llama3.2:3b | 0.837 s | 21.1 tok/s | 117 | 6.4 s |
| qwen2.5:7b | 1.877 s | 10.3 tok/s | 52 | 6.9 s |

### Parallel request test

Ollama handles concurrent requests fine. Two simultaneous `llama3.2:3b` requests completed in ~0.86 s wall time each (vs ~1.3 s sequential). So `OLLAMA_NUM_PARALLEL=2` works on CPU.

---

## Option B: Second Ollama GPU slot

**Won't work.** Only ~631 MB VRAM is free with the brain model loaded. Even a 1B model needs ~1-2 GB to load. OLLAMA_NUM_PARALLEL with GPU offloading is not feasible.

---

## Option C: Anthropic Haiku 5.5 API (cloud fallback)

### Latency

| Prompt type | Latency (avg) | Input tokens | Output tokens |
|---|---|---|---|
| Simple (2+2) | 0.677 s | 24 | 13 |
| Complex (build status query) | 2.277 s | 67 | 256 |

### Cost

| Metric | Value |
|---|---|
| Cost per query | $0.00007 |
| Cost per 1,000 queries | $0.07 |
| Cost per 10,000 queries | $0.71 |

Haiku 5.5 pricing: $0.80/MT input, $4.00/MT output.
At typical Discord usage (a few dozen messages per round), cost is negligible.

---

## Summary Comparison

| Option | TTFT (chat) | Response quality | Cost | GPU impact |
|---|---|---|---|---|
| llama3.2:1b CPU | ~0.5 s | Low (1B params) | Free | None |
| llama3.2:3b CPU | ~0.8 s | Medium (3B params) | Free | None |
| qwen2.5:7b CPU | ~1.9 s | Medium-High (7B params) | Free | None |
| Haiku 5.5 API | ~0.7 s (simple) / ~2.3 s (complex) | High (frontier model) | ~$0.07/1k queries | None |

## Recommendation

**llama3.2:3b on CPU** is the best default:
- Sub-second TTFT for short replies
- 21 tok/s is fast enough for a 50-100 token Discord message (~3-5 seconds total)
- Zero cost, zero GPU impact
- Handles parallel requests fine

**Haiku 5.5 API** as a quality fallback for when the user asks complex questions that need a smarter model. Cost is negligible at Discord chat volumes.

For the "front-desk" pattern (R1.5b): use llama3.2:3b for instant acknowledgment and simple replies, escalate to Haiku or the brain for anything requiring reasoning or build-system knowledge.
