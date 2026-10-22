# Thinking ON vs OFF Comparison

**Date:** 2026-10-21
**Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama 5 runs each
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM)
**Prompt:** "What is the capital of France? Answer with just the city name."

## Motivation

The qwen3.6:27b model supports a thinking mode that can add significant latency
before the first response token. This test compares thinking enabled vs disabled
to determine the tradeoff between response quality and latency.

## Results

### Time to First Content Token (what the user hears first)

| Metric | Thinking ON | Thinking OFF | Difference |
|--------|-------------|--------------|------------|
| Mean | 2.691s | 2.402s | 0.289s |
| P50 | 1.631s | 1.455s | |
| Min | 1.52s | 1.007s | |
| Max | 6.888s | 6.653s | |
| Std Dev | 2.348s | 2.387s | |

### Time to First Token (thinking or content)

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Mean | 1.33s | 1.338s |
| P50 | 0.246s | 0.243s |

### Total Response Time

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Mean | 2.708s | 2.413s |
| P50 | 1.658s | 1.483s |

### Tokens

| Metric | Thinking ON | Thinking OFF |
|--------|-------------|--------------|
| Thinking Tokens | 137 avg | 110 avg |
| Content Tokens | 1 avg | 1 avg |

### Sample Responses

**Thinking ON:**
- Thinking: Here's a thinking process:

1.  **Analyze User Input:**
   - Question: "What is the capital of France?"
   - Constraint: "Answer with just the city na
- Content: Paris

**Thinking OFF:**
- Thinking: Here's a thinking process:

1.  **Analyze User Input:**
   - Question: "What is the capital of France?"
   - Constraint: "Answer with just the city na
- Content: Paris

## Per-Run Data

### Thinking ON

| Run | First Token (s) | First Content (s) | Total (s) | Thinking Tok | Content Tok |
|-----|-----------------|-------------------|-----------|--------------|-------------|
| 1 | 0.246 | 1.631 | 1.658 | 143 | 1 |
| 2 | 5.677 | 6.888 | 6.888 | 112 | 1 |
| 3 | 0.242 | 1.625 | 1.653 | 140 | 1 |
| 4 | 0.249 | 1.52 | 1.548 | 131 | 1 |
| 5 | 0.238 | 1.793 | 1.793 | 161 | 1 |

### Thinking OFF

| Run | First Token (s) | First Content (s) | Total (s) | Thinking Tok | Content Tok |
|-----|-----------------|-------------------|-----------|--------------|-------------|
| 1 | 0.239 | 1.603 | 1.631 | 141 | 1 |
| 2 | 5.724 | 6.653 | 6.653 | 95 | 1 |
| 3 | 0.244 | 1.292 | 1.292 | 109 | 1 |
| 4 | 0.243 | 1.007 | 1.007 | 81 | 1 |
| 5 | 0.24 | 1.455 | 1.483 | 125 | 1 |

## Analysis

### The `think: false` Option Does Not Suppress Thinking

The most important finding: **setting `think: false` does not eliminate thinking tokens**. The model still produced 110 avg thinking tokens (vs 137 with thinking ON). This is a model-level behavior — the qwen3.6:27b model in the brain.Modelfile doesn't have a `think` parameter set, and Ollama's API option `think: false` doesn't override the model's built-in reasoning behavior.

### Latency Impact

- **Thinking ON adds ~0.3s** (2.691s vs 2.402s mean) to time-to-first-content.
- At P50: 1.631s (ON) vs 1.455s (OFF) — about **0.18s difference**.
- First thinking token arrives at ~0.24s regardless of the `think` setting.
- The high std-dev (2.3s+) is driven by outlier runs (#2 in each set took ~5.7s for first token, likely a GPU memory page fault).

### Why So Slow Compared to Wake-Latency Measurements

The previous wake-latency measurements (0.113s first output) used a different prompt and the model produced only 8 total tokens. Here, the model generates ~140 thinking tokens before any content — each thinking token takes ~10ms at ~100 tokens/s.

### Token Budget

With `num_predict=200`, the model uses ~140 tokens for thinking and only 1 for content. The thinking tokens consume most of the generation budget.

## Conclusion

### Should We Disable Thinking?

**Not for this model.** The `think: false` option doesn't work — the model still reasons internally. Disabling it saves ~0.18s at median, which is small relative to the ~1.5s total time-to-content. The latency is dominated by the number of thinking tokens the model generates, not by the thinking mode itself.

### Where to Look for Real Latency Reductions

1. **Model-level `think` parameter**: Set `PARAMETER think false` in the Modelfile itself (not just the API option) — this may work differently than the per-request option.
2. **Smaller model**: A 7B-12B model would generate fewer thinking tokens and be faster per-token.
3. **Prompt engineering**: Prepend "Answer directly without reasoning steps" to reduce thinking token count.
4. **`num_predict` tuning**: Lower values cut off thinking early but risk truncating the response.

### Recommendation

Keep thinking ON for now. The ~0.18s median saving from `think: false` is not worth the potential quality loss, especially since the option doesn't fully suppress reasoning on this model. If sub-second response times become critical, explore a smaller model or prompt-level thinking suppression.
