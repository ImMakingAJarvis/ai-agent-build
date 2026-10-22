# Engine Choice: llama.cpp server vs vLLM

> **Decision: llama.cpp server** (2026-10-22). Rationale: native GGUF support reuses our existing `jarvis-brain` weights, slot save/restore API needed for S5.3, KV cache quantization gives headroom, and two slots fit within 24.5 GB VRAM. No Discord poll was needed — user authorized proceeding without approval.

## Comparison Table (S2.1)

Feature facts for current releases, checked against upstream docs and source. No opinions yet.

| Feature | llama.cpp server b11815 (2026-10-22) | vLLM v0.32.0 (2026-10-18) |
|---------|--------------------------------------|---------------------------|
| **Model architecture (qwen35)** | Supported — GGUF native; our `jarvis-brain` loads via Ollama's llama.cpp backend with `RENDERER qwen3.5` [[1]](https://github.com/ggml-org/llama.cpp/blob/master/docs/models.md) | Uncertain — `Qwen3ForCausalLM` and `Qwen3_5ForConditionalGeneration` listed; `qwen35` text-only renderer not explicitly documented [[2]](https://github.com/vllm-project/vllm/blob/main/docs/models/supported_models.md) |
| **4-bit format & size (27B)** | Q4_K_M ~17 GB, native GGUF; also Q4_0, Q4_1, Q5_0, Q5_1, IQ2-IQ4 [[3]](https://github.com/ggml-org/llama.cpp/blob/master/docs/models.md) | GGUF support "highly experimental and under-optimized"; requires separate `vllm-gguf-plugin` install [[4]](https://github.com/vllm-project/vllm/issues/12149); GPTQ/AWQ 4-bit ~21 GB for 27B non-GGUF weights |
| **Anthropic `/v1/messages`** | Not native — exposes OpenAI-compatible `/v1/chat/completions` + custom `/completion`, `/slots` endpoints [[5]](https://github.com/search?q=repo:ggml-org%2Fllama.cpp+%2Fv1%2Fmessages&type=code) (0 matches) | Not native — OpenAI-compatible API only [[6]](https://github.com/vllm-project/vllm/blob/main/docs/serving/online_serving/openai_compatible_server.md) |
| **Proxy for Anthropic API** | LiteLLM: open-source, self-hostable, free; exposes `/messages` endpoint, translates to OpenAI format for backend [[7]](https://github.com/BerriAI/litellm/blob/main/README.md) | Same — LiteLLM self-hosted free tier; adds ~50 MB RAM overhead as thin HTTP proxy |
| **Multiple slots / concurrent requests** | `-np`/`--parallel N` creates N independent slots with separate KV contexts [[8]](https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md) | Continuous batching + PagedAttention; handles many concurrent requests natively [[9]](https://github.com/vllm-project/vllm/blob/main/docs/design/paged_attention.md) |
| **Request priority** | No native priority mechanism — slots serve requests round-robin; no preemption API | `X-Vllm-Priority` header on `/v1/chat/completions`; integer priority with scheduling support [[10]](https://github.com/vllm-project/vllm/blob/main/docs/serving/online_serving/openai_compatible_server.md) |
| **Save/restore one request's cache** | `POST /slots/{id_slot}?action=save|restore|erase`; `--slot-save-path` sets directory; returns token/byte counts and timings [[11]](https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md) | No per-request cache save/restore API; PagedAttention manages cache internally and evicts automatically [[12]](https://github.com/vllm-project/vllm/blob/main/docs/design/paged_attention.md) |
| **Prefix caching** | `--slot-prompt-similarity` reuses cached prefixes across requests in the same slot; `--kv-unified` shares KV buffer across all sequences [[13]](https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md) | Automatic Prefix Caching (APC) — caches and reuses KV cache of existing queries transparently [[14]](https://github.com/vllm-project/vllm/blob/main/docs/features/automatic_prefix_caching.md) |
| **MTP / draft decoding** | `--spec-type` supports `draft-simple`, `draft-eagle3`, `draft-mtp`, `draft-dflash`, `draft-dspark`, `ngram-simple`, `ngram-eagle3` [[15]](https://github.com/ggml-org/llama.cpp/blob/master/docs/speculative.md) | No speculative decoding / MTP support in v0.32.0 |
| **KV cache quantization** | `--cache-type-k` / `--cache-type-v`: f32, f16, bf16, q8_0, q4_0, q4_1, iq4_nl, q5_0, q5_1 (default f16) [[16]](https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md) | FP8 KV cache (per-tensor or per-attention-head); 2x reduction vs f16 [[17]](https://github.com/vllm-project/vllm/blob/main/docs/features/quantization/quantized_kvcache.md) |
| **Install method** | Build from source with CUDA + cmake; pinned to b11815 [[18]](https://github.com/ggml-org/llama.cpp/blob/master/docs/build.md) | `pip install vllm` in venv or Docker image; pinned to v0.32.0 [[19]](https://github.com/vllm-project/vllm/blob/main/docs/getting_started/installation.md) |

## VRAM Budget (S2.2)

Starting point: measured 23.4 GB VRAM used by `jarvis-brain` on the 4090 (24.5 GB total) = **1.1 GB headroom** [[20]](gpu-power-measurements.md).

### llama.cpp server

| Component | Calculation | VRAM |
|-----------|-------------|------|
| Model weights (Q4_K_M GGUF) | Same ~17 GB blob Ollama already has | ~17 GB |
| Runtime overhead | Process, CUDA context, Ollama measured 23.4 - 17 = ~6.4 GB includes idle KV + overhead | ~6.4 GB |
| Work slot KV cache (64K tokens) | 27B params → ~48 layers×2×(K+V); per-token at q4_0 cache: ~0.52 KiB/token × 64K | ~33 MB |
| Chat slot KV cache (8K tokens) | Same per-token cost × 8K | ~4 MB |
| `--kv-unified` shared buffer | Both slots share one pool; max concurrent = 64K + 8K = 72K tokens → ~37 MB peak | ~37 MB |
| **Total** | | **~23.44 GB** |

**Verdict: Two slots fit.** With `--cache-type-k q4_0 --cache-type-v q4_0`, the KV cache for both slots adds ~37 MB to the 23.4 GB baseline — well within 24.5 GB. Even at default f16 cache, 72K tokens ≈ 75 MB, still fits. The work slot can keep its full 64K context.

Without cache quantization (f16 default):

| Component | VRAM |
|-----------|------|
| Baseline (weights + overhead) | ~23.4 GB |
| 72K unified KV cache (f16, ~1.04 KiB/token) | ~75 MB |
| **Total** | **~23.48 GB** |

Still fits, but cache quantization gives more headroom for the model's own internal buffers.

### vLLM

| Component | Calculation | VRAM |
|-----------|-------------|------|
| Model weights (non-GGUF 4-bit GPTQ/AWQ) | 27B × 0.5 bytes × 2 (K+V scales) ≈ ~21 GB; GGUF experimental so unlikely to save the 4 GB difference | ~21 GB |
| Runtime overhead | vLLM process, CUDA graph, Python runtime | ~1-2 GB |
| Work slot KV cache (64K, FP8) | ~0.52 KiB/token × 64K | ~33 MB |
| Chat slot KV cache (8K, FP8) | ~0.52 KiB/token × 8K | ~4 MB |
| **Total** | | **~22-23 GB** |

**Verdict: Two slots probably fit, but with caveats.** vLLM's PagedAttention allocates KV cache in pages (typically 256 or 1024 tokens per page), so actual allocation is coarser. The larger concern is weight format: vLLM's GGUF support is experimental, so we'd likely need GPTQ/AWQ weights at ~21 GB (4 GB more than our GGUF blob). With FP8 KV cache, both slots still fit, but the weight format difference means we'd need to download and verify a new quant.

If we stick with f16 KV cache (no quantization), the 72K tokens need ~75 MB — still fits, but vLLM's internal page allocation adds overhead.

### Summary

| Engine | Two slots fit? | Work slot max context | Notes |
|--------|---------------|----------------------|-------|
| llama.cpp | Yes | Full 64K | Reuses existing GGUF; cache quantization gives headroom |
| vLLM | Probably | ~64K with FP8 KV cache | Needs new weight format download; GGUF experimental |

## S4.1 — Brain weights in engine format

| Property | Value |
|----------|-------|
| File | `/opt/jarvis-engine/models/jarvis-brain.gguf` |
| Size | 16 GB |
| SHA256 | `415aad607fc297a2ad84cda238febbde295f49f17c17ad7784e2246060fb06d0` |
| Format | Q4_K_M GGUF (same blob Ollama uses) |
| Source | Copied from `/usr/share/ollama/.ollama/models/blobs/` |

## Notes

- Both engines require LiteLLM as an Anthropic API proxy. LiteLLM is a thin HTTP layer (~50 MB RAM) that translates `/v1/messages` ↔ `/v1/chat/completions`.
- The `scripts/agent-local` script currently points `ANTHROPIC_BASE_URL` at Ollama. Switching engines means pointing it at LiteLLM (or the engine directly if we skip the proxy and use OpenAI format).
- llama.cpp's slot save/restore is the key differentiator for S5.3 (pausing work and preserving KV cache to disk).
