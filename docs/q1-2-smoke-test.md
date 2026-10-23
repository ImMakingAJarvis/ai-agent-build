# Q1.2 — Qwen 3.8 smoke test (Oct 23)

Model: `jarvis-brain-qwen38` (`qwen3.8:27b`, `num_ctx 65536`, `num_gpu 999`), loaded alone after `ollama stop jarvis-brain`. The 3.6 brain was reloaded afterwards, so nothing is switched yet.

| Check | Result |
|---|---|
| `agent-local check` (LOCAL_MODEL=jarvis-brain-qwen38) | PASS |
| `ollama ps` | 100% GPU, context 65536 |
| `/v1/messages` with a tool | PASS: `stop_reason: tool_use`, `get_weather {"city":"Paris"}` (plus a thinking block) |
| Decode speed (3 runs, 400 tokens, think off, temp 0.6) | 3.8: 82.0 / 83.9 / 84.9 tok/s (avg 83.6). 3.6: 82.2 / 90.8 / 82.2 (avg 85.1). Within 15%, about 2% slower. |
| `nvidia-smi` free VRAM ≥ 0.8 GB | **FAIL**: 117 MiB free (23964 of 24564 MiB used) |

## The VRAM criterion

The runner takes 22.4 GB with 3.8, the same as 3.6 (22.3 GB). With 3.6 loaded the card had only 209 MiB free. The other ~1.2 GB is desktop processes on the same GPU (gnome-remote-desktop 445 MiB, sunshine 394 MiB, Cursor, Steam, etc.), so 3.8 is no worse than 3.6, but the 0.8 GB headroom target is not met on this machine as configured. Temperature and the runner-up tag do not affect it, so I did not retry them.

Options for Nathan: accept parity with 3.6; quit sunshine/gnome-remote-desktop for headroom; or lower `num_ctx` / quantize the KV cache (changes the brain's setup, so not done here).

Q1.2 is therefore not fully done. Q1.3 should wait for Nathan's decision on this.
