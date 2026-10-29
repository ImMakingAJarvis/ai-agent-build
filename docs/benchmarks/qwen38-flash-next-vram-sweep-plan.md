# Qwen3.8-Flash-Next VRAM cache sweep — plan

Kanban epic: `f4cf6031` — "vram cache benchmarking of MoE models"
(asked 2026-10-27: *set up Qwen 3.8 Flash Next MoE with FreeToken; test the
VRAM cache amount which maximizes token speed, prompt speed, and context
window*).

## The sweep

One benchmark point per job — engine sized via FreeToken
`--memory-ratio` so that a fixed amount of VRAM stays free after model
load. The engine's expert cache / KV then gets whatever is left, which is
the variable under test.

| Job | Kanban task | Free VRAM target | Result file |
|---|---|---|---|
| `moe-bench-100` | `066c8a01` | 100 MB | `qwen38-flash-next-vram-sweep-100mb.md` |
| `moe-bench-150` | `69b984cf` | 150 MB | `qwen38-flash-next-vram-sweep-150mb.md` |
| `moe-bench-200` | `ccb3a8f3` | 200 MB | `qwen38-flash-next-vram-sweep-200mb.md` |
| `moe-bench-250` | `5669329e` | 250 MB | `qwen38-flash-next-vram-sweep-250mb.md` |

(Nathan's original ask also listed 300 MB; no kanban task was created for
it — add one and a matching job if it is wanted.)

All four jobs were enqueued 2026-10-29 (job `chat-1793301484-1e78`) as
`build`-class, priority 5, driver `sonnet` — the sweep must run on an API
driver because it stops the local brain engine.

## Method (per point)

1. `docker compose stop engine` — the brain llama-server is the compose
   container `jarvis-engine` on :11434 (the legacy systemd
   `jarvis-engine.service` is stopped/legacy, do not start it). Verify
   ~24 GB free of the 24,564 MiB card.
2. Start: `/home/agent/ftvenv/bin/ft serve --model
   /home/agent/models/qwen3.8-flash-next/UD-IQ4_XS --port 11436
   --moe-strategy hybrid --moe-cache-auto --memory-ratio <r>` — pick `r`
   so free VRAM lands at the target ± 40 MB (start ≈ 0.98–0.99, iterate).
   Port 11435 is the iGPU helper — never use it.
3. 3 runs of the standard prompt (`moe-sweep-prompt.txt` next to this
   file — identical across all four points), OpenAI-compatible
   `/chat/completions`, non-streaming, temperature 0.7. Record prompt
   tokens/s, generation tokens/s, total time per run (run 1 = cold).
4. Max context: grow the prompt until the server refuses/errors; record
   the largest prompt served at this cache size.
5. Kill `ft serve`; `docker compose start engine`; verify brain answers
   on :11434 and iGPU helper on :11435.
6. Results to the point's own file above (one file per point, so the four
   PRs never conflict); final cross-point comparison when all four land.

Reference numbers from the Ollama MoE bench
(`docs/moe-benchmark.md`): Qwen3 MoE 30B-A3B did ~212 tok/s generation,
~162K tok/s cached prompt at ~300 MB GPU overhead — the FreeToken points
should be compared against those, not only against each other.

## Caveats

- The card OOMs if `ft serve` starts while the brain engine is up — step 1
  is mandatory, and the local model (including this agent's own brain) is
  down for the duration of each point.
- The model is a 3-part split GGUF (~100 GB on disk, experts on NVMe via
  MoE offload); pass the folder or first shard.
- `jarvis-moe-engine.service` (old 0.1.1 venv) stays stopped.
