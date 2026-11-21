# MoE 128K context suite — optimal cache size, plan

Job `chat-1795017563-20e9` (2026-11-18, Nathan via Discord): *plan and do a
full benchmark suite to find the optimal cache size for all MoE models with
128K context, and add results to the benchmark page. If any results seem
promising, install that model as the thinker/planner/coder if it's a good
coder. Part of the benchmark should be coding exercises.*

Plane project: [CTX](http://111.1.2.222:8088/jarvis/browse/CTX-1) — "MoE
128K context suite (optimal cache size)", created 2026-11-18.

## What "optimal cache size" means here

The earlier sweeps (`qwen38-flash-next-vram-sweep-*`, `glm53-flash-vram-sweep-*`)
varied the free-VRAM target (100–400 MB) at default context: the expert
cache + KV got whatever was left. This suite fixes the context at **128K
(131072 max seq len, KV reserved for 128K tokens)** and sweeps the same
free-VRAM ladder. With 128K KV reserved, less free VRAM = a bigger expert
cache = fewer PCIe expert fetches = faster generation, up to the OOM floor
(GLM's KDA Triton autotune alone needs ~256 MiB scratch, so ~300 MB free is
its practical floor — see `glm53-flash-vram-sweep-250mb.md`).

**Optimal point per model = the lowest free-VRAM ladder point at which the
whole suite (all three components) completes.** The answer recorded for each
model is the `moe_cache_size` (MiB) the engine actually resolved at that
point — that is the "optimal cache size" the job asks for.

## Models

| Model | Quant | Path |
|---|---|---|
| Qwen 3.8 Flash Next (MoE) | UD-IQ4_XS | `/home/agent/models/qwen3.8-flash-next/UD-IQ4_XS` (3-part GGUF, first shard) |
| GLM 5.3 flash (MoE) | UD-IQ2_XXS | `/home/agent/models/glm5.3-flash/UD-IQ2_XXS` (first shard) |

## The suite (harness in `bench/coding/`, PR from 2026-11-18)

Three components per cache point, all against `ft serve` on :11436
(OpenAI-compatible `/chat/completions`):

1. **Standard 3-run prompt sweep** — `run_standard.py`, the same
   `moe-sweep-prompt.txt` as the VRAM sweeps, so numbers stay comparable
   (run 1 cold; prompt tok/s from a `max_tokens=1` call, generation tok/s
   = 256 / (total − prefill)).
2. **Coding battery** — `run_exercises.py`: five exercises (LRU cache,
   merge intervals, topo sort, s-expression evaluator, LIS; 86 test checks
   total), one generation each, graded by the exercise's tests. This is
   the "is it a good coder" evidence for the thinker/planner/coder decision.
3. **128K needle** — `needle_128k.py`: deterministic ~120K-token document
   (length calibrated against the endpoint's own tokenizer), needle line
   at 0/25/50/75/100%, exact-containment scoring. Proves the full window
   is served at this cache size; a position that errors is a real result.

## Ladder and point order

Same free-VRAM targets as the VRAM sweeps (± 40 MB, `--memory-ratio`
calibrated per start): **400, 300, 250, 200, 150, 100 MB** — one Plane
item each per model, parented to CTX-1 (12 items total, **CTX-2…CTX-13**,
created up front in ladder order by the 2026-11-21 decomposition round).
Each igpu point job posts progress on its own item (see Driver
correction #2).

Run order per model (minimize restarts, validate the harness first):
**400 → 100 → 200 → 250 → 300 → 150**. If 400 MB cannot serve 128K at all,
the model fails the suite — record why and stop (no smaller point can
work: less free VRAM means less total headroom). If 100 MB passes, it is
the optimal point; the intermediate points are still run for the
speed-vs-cache table.

## Method (per point)

1. `docker compose stop engine` — done **inside the point script**
   (`scripts/moe-128k-point.sh`), which runs from an igpu point job.
   (The rule is that the *local brain round* never stops the brain; an
   igpu/sonnet round may. See Driver correction #2.) Verify ~24 GB free
   of the 24,564 MiB card. The iGPU helper (`jarvis-engine-igpu`,
   :11435) stays up.
2. Start `ft serve` with 128K context. Base invocation (first shard as
   model path, port 11436 — never 11435):

   ```bash
   PATH=/home/agent/ftvenv/bin:$PATH /home/agent/ftvenv/bin/ft serve \
     --model /home/agent/models/<model>/<quant>/<first shard>.gguf \
     --port 11436 --moe-strategy hybrid --moe-cache-auto \
     --max-seq-len-override 131072 --kv-reserve-tokens 131072 \
     --memory-ratio <r>
   ```

   (flag names per `/home/agent/freetoken/docs/cli.md`; if
   `--max-seq-len-override`/`--kv-reserve-tokens` are not the exact
   128K levers in 0.2.1, use `ft serve --help` to find them and record
   what was used. `ninja` must be on PATH.)
3. From the engine log / `ft ctl`, record the **resolved** `moe_cache_size`
   (MiB) and KV capacity for the start — the cache size is the dependent
   variable here, so every point reports its resolved value.
4. Run the suite into
   `~/.local/share/jarvis/bench/moe-128k/<model-slug>-<point>/`:
   `standard.json`, `exercises.json`, `needle.json` (README in
   `bench/coding/`).
5. Kill `ft serve` by the recorded PID (never `pkill -f "ft serve"` —
   it matches other processes; orphan `spawn_main` workers hold ~21 GB).
   `docker compose start engine`; verify the brain answers on :11434 and
   the iGPU helper on :11435 **before the round ends**.
6. Results doc `docs/benchmarks/moe-128k-<model-slug>-<point>mb.md` per
   point (transcribed tables + resolved cache size + failures verbatim),
   mirroring the VRAM-sweep point docs.

If a round runs long, end it at a point boundary with the continuation
filled (what's done, what's next) and let the next igpu round continue —
the point docs are the durable progress record.

## After the GPU phase (local rounds)

1. **Per-model summary** `docs/benchmarks/moe-128k-suite-results.md`:
   speed-vs-cache tables (standard sweep + needle prefill), the optimal
   point per model with its resolved `moe_cache_size`, the coding-battery
   pass rates per point (they should be context-length effects, not
   cache-size effects — if the battery score drops at a point, that is a
   finding).
2. **Dashboard**: the numbers are transcribed into
   `internal/dashboard/moebench.go` (the `/benchmarks` page is a static
   Go report, not file-backed) as a new "128K suite" section per model.
3. **Good-coder decision**: from the coding battery + the standard prompt
   (which is a code-review task), decide per model whether it is a good
   enough coder to install as thinker/planner/coder. If yes: registry
   entry in `config/models.yaml` (flip `enabled` after the install),
   the engine/config change for the role in question, and a results note
   with the evidence. If no: record why in the summary doc.

## Safety (repeated because the sweeps learned it the hard way)

- Brain engine stopped **only** in the igpu round; restored + verified
  before the round ends. The watchdog maintenance window re-arms the
  brain at the deadline no matter what.
- Port 11435 is the iGPU helper — the suite targets 11436 only.
- Kill `ft serve` by PID; check `nvidia-smi` for orphan workers after.
- One results doc per point so parallel rounds never conflict.

## Round split

| Round | Driver | Work |
|---|---|---|
| 1 (2026-11-18) | local | Harness (exercises + 3 runners) + this plan + CTX project — the PR this doc lives in |
| 2 (2026-11-21) | local | Decomposition: the point script, 12 chained igpu point jobs, CTX-2…13 Plane items (this PR) |
| 3+ | **igpu** | The 12 ladder points — one self-contained point job each, running `scripts/moe-128k-point.sh <slug> <N>` |
| final | local | Summary + dashboard section + good-coder decision + install if warranted |

## Driver correction (2026-11-20)

The GPU phase runs on **sonnet**, not igpu. The igpu driver's 3B model
(32K ctx) cannot take the round prompt: `chat-1795017563-20e9` failed in
~2 s with "Prompt is too long" (2026-11-19), as the earlier
`moe-bench-200` point had. Every completed VRAM-sweep point
(`moe-bench-100/150/250`) ran on sonnet — a cloud round is free to stop
the brain container (the watchdog re-arms it; `ft serve` must be killed
by PID first, per Safety). `chat-1795017563-20e9` was requeued
`-driver sonnet` on 2026-11-20. Note the iGPU **engine** (:11435) stays
up through every point — "igpu" in the old table was a confusion of the
two.

## Driver correction #2 (2026-11-21) — decomposition to iGPU point jobs

Nathan asked (job `chat-1795228874-940b`) to break the suite into single
tasks small enough that the iGPU (3B, 32K ctx) can run each, via the
event-driven system. The 2026-11-20 sonnet correction is **superseded** —
the GPU phase now runs as **12 chained igpu point jobs**, one per ladder
point. Two things make it fit:

1. **Slim iGPU rounds.** `agent-local` now sets
   `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1` for igpu rounds (run.go,
   `defaultCommand`), skipping the repo + global CLAUDE.md files. That
   drops the round from ≈34.6K tokens (over the 3B's 32K window → "Prompt
   is too long" in ~2 s) to ≈21.6K first turn, which fits. The
   `stage3-igpu` brief is self-contained and carries the standing rules the
   CLAUDE.md files would have provided. Live-verified 2026-11-21:
   `agent-local check` against :11435 passes (the 3B drives the Bash tool
   in a slim round).
2. **One self-contained point per round.** Each ladder point is a single
   script, `scripts/moe-128k-point.sh <slug> <N>` (installed to
   `/usr/local/lib/jarvis/moe-128k-point.sh`), that does the whole point —
   stop the brain, start `ft serve` with the calibrated `--memory-ratio`,
   run the three runners, restore + verify the brain, write the point doc.
   The 3B makes a handful of tool calls (run the script, read the SUMMARY
   line, post the board comment, finish); it never reasons over the bench
   output.

The 12 point jobs (`moe128k-<slug>-<N>`, `-driver igpu -effort off`) are
chained with `-depends` in ladder order (400 → 100 → 200 → 250 → 300 →
150 per model, qwen before glm) so a failed point parks the rest
(stop-on-failure). A final `moe128k-summary` local job depends on the last
point and does the per-model summary + dashboard + good-coder decision.
Plane items **CTX-2…CTX-13** (created in the same ladder order) track each
point; each job comments/dones its own item, or moves it to `Blocked` on
failure.

**Pre-flight (2026-11-20, local round):** `ft` and `ninja` are in
`/home/agent/ftvenv/bin`; the installed `ft serve` has every flag this
plan uses (`--max-seq-len-override`, `--kv-reserve-tokens`,
`--memory-ratio`, `--moe-strategy hybrid`, `--moe-cache-auto`); both
first shards are on disk:
`Qwen3.8-Flash-Next-UD-IQ4_XS-00001-of-00003.gguf` and
`GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf`. The harness was
smoke-tested end-to-end against the iGPU endpoint (:11435, Qwen2.5-3B —
a harness check only, not a suite point; JSONs in
`~/.local/share/jarvis/bench/moe-128k/_smoke-igpu/`): all three runners
completed — standard 3/3 runs (1373 prompt tokens, ~13 gen tok/s),
exercises 5/5 executed and graded (2/5 passed, expected of a 3B model),
needle calibrated and 5/5 positions answered. All 86 exercise reference
checks pass offline.
