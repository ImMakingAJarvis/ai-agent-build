# MoE models to benchmark

The list of MoE models benchmarked on the 4090 with FreeToken — one
VRAM-cache sweep per model (board standard: one Plane project per epic).
Each model has its own sweep plan doc under `docs/benchmarks/` and a
results file per cache point.

| Model | Quant | Size | Plane project | Status (2026-11-04) |
|---|---|---|---|---|
| Qwen 3.8 Flash Next (MoE) | UD-IQ4_XS | ~100 GB (3-part GGUF) | [MOE](http://111.1.2.222:8088/jarvis/browse/MOE-1) | sweep running (PRs #157/#158); plan: `qwen38-flash-next-vram-sweep-plan.md` |
| GLM 5.3 flash (MoE) | TBD (pull pending) | TBD | [GLM](http://111.1.2.222:8088/jarvis/browse/GLM-1) | queued — model pull pending; plan: `glm53-flash-vram-sweep-plan.md` |

## Reference numbers

Sweep points are compared against each other AND against the Ollama MoE
bench (`docs/moe-benchmark.md`): qwen3:30b-a3b Q4 did ~212 tok/s
generation and ~162K tok/s cached prompt at ~300 MB GPU overhead.

## Adding a model to the list

1. Add a row to the table above.
2. `plane-epic create --identifier XXX "<title>" --description "..."`
   (flags BEFORE the title — Go's flag package stops at the first
   positional).
3. One work item per cache point (100/150/200/250/300/400 MB free),
   parented to the epic item.
4. A sweep plan doc `docs/benchmarks/<model-slug>-vram-sweep-plan.md`,
   mirroring `qwen38-flash-next-vram-sweep-plan.md`.
5. If the model is a candidate to actually run, a registry entry in
   `config/models.yaml` (`enabled: false` until pulled + benched).
