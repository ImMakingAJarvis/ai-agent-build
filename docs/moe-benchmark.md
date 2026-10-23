# MoE Benchmark Results

## Setup

| Parameter | Value |
|---|---|
| GPU | NVIDIA RTX 4090 (24.5 GB VRAM) |
| Ollama | 0.40.7 |
| GPU Overhead | 300 MB (`OLLAMA_GPU_OVERHEAD=314572800`) |
| Flash Attention | Enabled |
| Keep Alive | -1 (forever) |

## Models Compared

| Model | Architecture | Base | Quantization | VRAM | Context |
|---|---|---|---|---|---|
| **jarvis-moe** | Mixture of Experts | qwen3:30b-a3b | Q4 | ~23 GB | 49,152 |
| **jarvis-brain** | Dense | qwen3.6:27b | Q4_K_M | ~17 GB | 65,536 |

## Benchmark Methodology

- **Prompt:** 5,414 chars / 526 words (Python code review request with ~150 lines of code)
- **Runs:** 3 per model (first run = cold load, runs 2-3 = cached)
- **Measurement:** Ollama `/api/chat` non-streaming endpoint timing metadata
- **Temperature:** 0.7

## Results

### jarvis-moe (Qwen3 MoE 30B-A3B)

| Run | Prompt Tokens | Prompt Speed | Gen Tokens | Gen Speed | Total Time | Load |
|---|---|---|---|---|---|---|
| 1 (cold) | 1,191 | 4,818 tok/s | 9,052 | 211.9 tok/s | 47.90s | 4,914ms |
| 2 (cached) | 1,191 | 163,621 tok/s | 10,261 | 209.9 tok/s | 49.28s | 1ms |
| 3 (cached) | 1,191 | 160,016 tok/s | 7,705 | 214.4 tok/s | 36.37s | 1ms |
| **Avg** | **1,191** | **109,485 tok/s*** | **9,006** | **212.1 tok/s** | **44.52s** | — |

\* Prompt avg skewed by cold load; cached runs ~162K tok/s

### jarvis-brain (Qwen3.6 27B)

| Run | Prompt Tokens | Prompt Speed | Gen Tokens | Gen Speed | Total Time | Load |
|---|---|---|---|---|---|---|
| 1 (cold) | 1,299 | 1,924 tok/s | 5,581 | 103.6 tok/s | 58.75s | 4,205ms |
| 2 (cached) | 1,299 | 12,075 tok/s | 5,527 | 102.0 tok/s | 54.69s | 0ms |
| 3 (cached) | 1,299 | 12,390 tok/s | 6,245 | 103.1 tok/s | 61.03s | 0ms |
| **Avg** | **1,299** | **8,797 tok/s*** | **5,784** | **102.9 tok/s** | **58.16s** | — |

\* Prompt avg skewed by cold load; cached runs ~12.2K tok/s

## Summary

| Metric | jarvis-moe | jarvis-brain | Winner |
|---|---|---|---|
| Gen throughput | **212.1 tok/s** | 102.9 tok/s | **moe 2.06x** |
| Prompt throughput (cached) | **~162K tok/s** | ~12.2K tok/s | **moe 13.3x** |
| Avg gen tokens | **9,006** | 5,784 | **moe 1.56x** |
| Avg total time | **44.5s** | 58.2s | **moe 1.31x** |
| Context window | 49,152 | **65,536** | **brain 33% more** |
| VRAM used | ~23.5 GB | ~17 GB | brain uses less |
| Model size | ~18 GB | ~17 GB | similar |

## Analysis

The MoE model is **significantly faster** at generation (2.06x) despite being a larger parameter model. This is because MoE architecture only activates a subset of parameters per token (~3B active out of 30B total), making each forward pass cheaper than the dense 27B brain model.

The prompt processing speed difference is dramatic when cached (13.3x), though the brain's cached prompt speed (12.2K tok/s) is still adequate for most use cases.

The MoE model produces longer, more detailed responses (9K vs 5.8K tokens on average), which combined with higher throughput still results in faster total response time.

Trade-offs:
- MoE uses ~6.5 GB more VRAM, leaving less headroom
- MoE context is 49K vs 65K (still generous for most agent work)
- MoE is from the earlier Qwen3 family (not 3.6), so may differ in quality

## System Config

```ini
# /etc/systemd/system/ollama.service.d/jarvis.conf
[Service]
Environment="OLLAMA_KEEP_ALIVE=-1"
Environment="OLLAMA_FLASH_ATTENTION=1"
Environment="OLLAMA_GPU_OVERHEAD=314572800"
```

The `OLLAMA_GPU_OVERHEAD=314572800` (300 MB) reserves minimal VRAM for CUDA operations, allowing the MoE model + KV cache to use ~24.2 GB of the 24.5 GB GPU.
