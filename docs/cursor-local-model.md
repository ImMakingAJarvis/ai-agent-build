# Wiring a Local Coding Model into Cursor

## Summary

Cursor can be connected to a local Ollama instance as an OpenAI-compatible endpoint using the **Continue** extension. This allows routing boilerplate/repetitive coding tasks to a local model, reducing hosted API costs. The native Cursor AI chat does not support custom endpoints.

## Key Findings

### 1. Ollama OpenAI-Compatible API

Ollama exposes a fully OpenAI-compatible API at `http://localhost:11434/v1/`:

| Feature | Status | Notes |
|---------|--------|-------|
| `/v1/models` | Working | Lists all loaded models |
| `/v1/chat/completions` | Working | Standard OpenAI format |
| Streaming (`stream: true`) | Working | SSE format, works with Continue |
| Tool calling | Working | Function calling returns proper `tool_calls` |
| Thinking mode | Always on for qwen3.6:27b | `think: false` param not respected in OpenAI API; thinking consumes ~50% of tokens before producing output |

**Tested response time**: ~9 seconds for a medium coding task (Go function with error handling) on the current brain model.

### 2. Cursor Custom Model Support

Cursor (v3.24.18) does **not** have native support for custom OpenAI-compatible endpoints in its built-in AI chat. The built-in chat is locked to Cursor's hosted API (which wraps Claude/GPT).

**Solution: Continue extension**

[Continue](https://github.com/continuedev/continue) is an open-source VS Code extension that:
- Supports Ollama as a native provider (no API key needed)
- Supports custom OpenAI-compatible endpoints
- Replaces or supplements Cursor's built-in AI chat
- Supports tab autocompletion, chat, and code editing

**Configuration**: `~/.continue/config.json` (or `~/.continue/config.yaml`):
```json
{
  "models": [
    {
      "title": "Local Coder",
      "provider": "ollama",
      "model": "qwen3-coder:30b",
      "completionOptions": {
        "temperature": 0.3,
        "maxTokens": 4096
      }
    }
  ],
  "tabAutocompleteModel": {
    "title": "Local Autocomplete",
    "provider": "ollama",
    "model": "qwen2.5-coder:7b"
  }
}
```

### 3. Hardware Constraints

| Resource | Value |
|----------|-------|
| GPU | RTX 4090 (24.5 GB VRAM) |
| Current brain model (qwen3.6:27b) | 17 GB VRAM |
| Headroom | ~1.8 GB (not enough for a second large model) |

**Implication**: Models must be **swapped**, not co-loaded. The brain model must be unloaded before loading a coder model, and vice versa.

### 4. Available Coding Models (Ollama Library)

| Model | Size (Q4) | Fits? | Notes |
|-------|-----------|-------|-------|
| `qwen3-coder-next` | 51 GB | **No** | Too large for 24 GB GPU |
| `qwen3-coder:30b` | 18 GB | Yes | Best coding-focused option that fits |
| `qwen2.5-coder:32b` | ~20 GB | Tight | May leave too little headroom |
| `codestral:22b` | ~14 GB | Yes | Mistral's coding model, good quality |
| `deepseek-coder-v2:16b` | ~10 GB | Yes | Solid coding model |
| `starcoder2:15b` | ~9 GB | Yes | Good for autocomplete |
| `qwen2.5-coder:7b` | ~4.7 GB | Yes | Fast, good for autocomplete |

### 5. Model Swap Strategy

Since we can't co-load brain + coder models, the worker needs a model swap workflow:

```
1. Worker picks up a coding job
2. Stop brain model: ollama stop jarvis-brain
3. Pull/load coder model: ollama run qwen3-coder:30b
4. Run coding task via Ollama API
5. Unload coder: ollama stop qwen3-coder:30b
6. Reload brain: ollama run jarvis-brain
7. Resume normal operation
```

**Load time overhead**: ~15-30 seconds per swap (based on model-load-time-measurements.md data). This is acceptable for batch jobs but not for interactive use in Cursor.

### 6. Cursor Interactive Use Case

For **interactive** use in Cursor (not batch jobs), the model needs to stay loaded. Options:

1. **Dedicated coder model in Cursor**: Load a coding model in Ollama and point Continue at it. This means the brain model is offline while coding in Cursor.
2. **Use brain model for coding**: The current `qwen3.6:27b` is already marketed for agentic coding. Wire it through Continue and skip the separate coder model for now.
3. **Two-GPU setup**: Not applicable (single GPU).

## Recommendations

### Short-term (no new model download)

1. Install the **Continue** extension in Cursor
2. Configure it to use the existing `jarvis-brain` model via Ollama
3. Test coding quality for boilerplate tasks
4. If quality is acceptable, no need for a separate coder model

### Medium-term (if a dedicated coder model is needed)

1. Pull `qwen3-coder:30b` (18 GB, fits GPU)
2. Create a `coder.Modelfile` with optimized parameters (no thinking, lower temperature)
3. Build a model-swap script for the worker: `scripts/swap-model.sh brain|coder`
4. Configure Continue to use the coder model when loaded

### Long-term

1. Benchmark `qwen3-coder:30b` vs `jarvis-brain` on coding tasks
2. Evaluate whether the quality difference justifies the swap overhead
3. Consider a larger GPU or second GPU if both models need to coexist

## Implementation Steps

### Step 1: Install Continue Extension in Cursor

1. Open Cursor → Extensions (Ctrl+Shift+X)
2. Search for "Continue" by Continue Dev
3. Install and restart Cursor

### Step 2: Configure Continue

Create `~/.continue/config.json`:
```json
{
  "models": [
    {
      "title": "Jarvis Brain (Local)",
      "provider": "ollama",
      "model": "jarvis-brain",
      "completionOptions": {
        "temperature": 0.3,
        "maxTokens": 8192
      }
    }
  ]
}
```

### Step 3: Test

Open Cursor's Continue chat (Ctrl+L) and verify the local model responds.

### Step 4: Dedicated Coder Model (Optional)

If a dedicated coder model is needed:
```bash
ollama pull qwen3-coder:30b
```

Create `models/coder.Modelfile`:
```
FROM qwen3-coder:30b
PARAMETER num_ctx 32768
PARAMETER num_gpu 999
PARAMETER temperature 0.3
PARAMETER top_p 0.9
```

```bash
ollama create jarvis-coder -f models/coder.Modelfile
```

## Open Questions

1. **Continue vs Cursor native AI**: Can Continue fully replace Cursor's built-in AI, or do we lose features (like Cursor's codebase indexing)?
2. **Model swap automation**: Should the worker automatically swap models, or should we keep the brain always loaded and accept slower coding responses?
3. **Quality benchmark**: Need to benchmark `qwen3-coder:30b` vs `qwen3.6:27b` on actual project coding tasks before committing to a swap strategy.
