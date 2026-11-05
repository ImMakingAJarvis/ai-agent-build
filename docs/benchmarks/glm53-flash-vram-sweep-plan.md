# GLM 5.3 flash VRAM cache sweep — plan

Plane project: `GLM` — "vram cache benchmarking of GLM 5.3 flash"
(added 2026-11-04, job `chat-1793833477-49fb`: GLM 5.3 flash joins the
MoE benchmark list, gets its own project, cache-point tests and a
registry entry).

Same design as the Qwen 3.8 Flash Next sweep
(`qwen38-flash-next-vram-sweep-plan.md`): one benchmark point per job —
engine sized via FreeToken `--memory-ratio` so that a fixed amount of
VRAM stays free after model load. The engine's expert cache / KV then
gets whatever is left, which is the variable under test.

## Prerequisite — pull the model

GLM 5.3 flash is not pulled yet (only `/home/agent/models/
qwen3.8-flash-next` is on disk as of 2026-11-04). First run of this
sweep must:

1. Pick the GGUF source + quant for Zhipu's GLM 5.3 flash that fits the
   hybrid strategy (weights mostly RAM-resident, experts hot on the
   24.5 GB card) — record the exact repo/tag/quant in the 100 MB point's
   results file.
2. Download under `/home/agent/models/glm5.3-flash/<QUANT>/`.
3. Verify resolution: `ft bench bw --model <first shard or folder>`.

If no quant fits the hybrid strategy in 24.5 GB VRAM + 128 GB RAM,
record why in the results and stop — do not force a CPU-only run into
this sweep.

## The sweep

One benchmark point per job, parented to the epic item GLM-1:

| Point | Plane item | Free VRAM target | Result file |
|---|---|---|---|
| 1 | GLM-2 | 100 MB | `glm53-flash-vram-sweep-100mb.md` |
| 2 | GLM-3 | 150 MB | `glm53-flash-vram-sweep-150mb.md` |
| 3 | GLM-4 | 200 MB | `glm53-flash-vram-sweep-200mb.md` |
| 4 | GLM-5 | 250 MB | `glm53-flash-vram-sweep-250mb.md` |
| 5 | GLM-6 | 300 MB | `glm53-flash-vram-sweep-300mb.md` |
| 6 | GLM-7 | 400 MB | `glm53-flash-vram-sweep-400mb.md` |

(Points 5–6 match the extra MOE-project points added after the original
100/150/200/250 set, so the two sweeps stay comparable.)

## Method (per point)

1. `docker compose stop engine` — the brain llama-server is the compose
   container `jarvis-engine` on :11434. Verify ~24 GB free of the
   24,564 MiB card.
2. Start: `/home/agent/ftvenv/bin/ft serve --model
   /home/agent/models/glm5.3-flash/<QUANT> --port 11436 --moe-strategy
   hybrid --moe-cache-auto --memory-ratio <r>` — pick `r` so free VRAM
   lands at the target ± 40 MB (start ≈ 0.98–0.99, iterate). Port 11435
   is the iGPU helper — never use it.
3. 3 runs of the standard prompt (`moe-sweep-prompt.txt` next to this
   file — identical across all points AND across models), OpenAI-
   compatible `/chat/completions`, non-streaming, temperature 0.7.
   Record prompt tokens/s, generation tokens/s, total time per run
   (run 1 = cold). If the standard prompt OOMs at this cache size,
   record that and use the largest prompt that fits (the Qwen 100/150 MB
   points OOMed — see PRs #157/#158).
4. Max context: grow the prompt until the server refuses/errors; record
   the largest prompt served at this cache size.
5. Kill `ft serve`; `docker compose start engine`; verify brain answers
   on :11434 and iGPU helper on :11435.
6. Results to the point's own file above (one file per point, so the
   six PRs never conflict); cross-point comparison when all six land,
   plus a model-vs-model table against the Qwen 3.8 Flash Next points.

## Reference numbers

- Ollama MoE bench (`docs/moe-benchmark.md`): qwen3:30b-a3b Q4, ~212
  tok/s generation, ~162K tok/s cached prompt at ~300 MB GPU overhead.
- Qwen 3.8 Flash Next FreeToken sweep: `qwen38-flash-next-results.md` +
  per-point files.

## Caveats

- The card OOMs if `ft serve` starts while the brain engine is up —
  step 1 is mandatory, and the local model (including this agent's own
  brain) is down for the duration of each point.
- `jarvis-moe-engine.service` (old 0.1.1 venv) stays stopped.
