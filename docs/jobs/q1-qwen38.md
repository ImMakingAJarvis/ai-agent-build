# Q1 — Upgrade the local brain from Qwen 3.6 to Qwen 3.8

Read only the section for your job.

Background:
- Ollama 0.40.7 serves the brain as `jarvis-brain`, built from `models/brain.Modelfile` (`FROM qwen3.6:27b`, Q4_K_M, `num_ctx 65536`, `num_gpu 999`, MTP draft). `local.env` has `LOCAL_MODEL=jarvis-brain`. The worker and the idle unloader use that name too.
- VRAM on Oct 21: about 22.3 GB for the runner, and 23.6 of 24.5 GB used on the card.

Target: `qwen3.8:27b`, which is the same as `latest` and `27b-mtp-q4_K_M`. It is a 27B Q4_K_M model with the same 16.8 GB weights size as 3.6, plus a 0.93 GB projector and MTP draft, and needs Ollama 0.32.12 or later.
- Runner-up: `qwen3.8:27b-q4_K_M`, the same weights without the draft parameters.
- Q8_0 (30 GB) and BF16 do not fit, and the MLX and nvfp4 tags are not for this runtime.

The brain can't test its own replacement while it is loaded, because two copies don't fit in VRAM. Run Q1.2 and Q1.3 on Sonnet, with `!pause jobs` (or the worker stopped) during the swap.

## Q1.1 — Pull and prepare (local model is fine)

- `ollama pull qwen3.8:27b`, then `ollama cp jarvis-brain jarvis-brain-qwen36` as the rollback copy. Add `models/brain-qwen38.Modelfile` (`FROM qwen3.8:27b`, `num_ctx 65536`, `num_gpu 999`) and `ollama create jarvis-brain-qwen38`. Don't load it.
- Done when: `ollama list` shows `qwen3.8:27b`, `jarvis-brain-qwen38` and `jarvis-brain-qwen36`, and a PR adds the Modelfile.

## Q1.2 — Smoke test (Sonnet)

- `LOCAL_MODEL=jarvis-brain-qwen38 agent-local check` passes. `ollama ps` shows 100% GPU at context 65536, and `nvidia-smi` shows at least 0.8 GB free. A `/v1/messages` request with a tool returns a well-formed `tool_use`. Decode speed is within 15% of 3.6. Record the numbers in the PR. If tool calls misbehave, retry with `temperature 0.6`, then with the runner-up tag.
- Done when: all of the above pass and are recorded.

## Q1.3 — Switch, with tested rollback (Sonnet, ask Nathan first)

- `ollama cp jarvis-brain-qwen38 jarvis-brain && ollama stop jarvis-brain`, then run `agent-local check`. Set `models/brain.Modelfile` to `FROM qwen3.8:27b`. Test the rollback once (`ollama cp jarvis-brain-qwen36 jarvis-brain && ollama stop jarvis-brain && agent-local check`), then switch forward again. Keep `qwen3.6:27b` until Nathan says to remove it.
- Done when: both checks pass, `docs/local-brain.md` is updated (model, VRAM, why, rollback), and Nathan gets a notify with VRAM before and after.
