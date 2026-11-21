# MoE 128K suite — benchmark harness

Harness for the "optimal cache size at 128K context" benchmark
(`docs/benchmarks/moe-128k-suite-plan.md`). Three runners, all
dependency-free (stdlib only), all talking OpenAI-compatible
`/chat/completions` — the FreeToken `ft serve` box on **port 11436**
while it is up. The 4090 must be free of the brain engine for the
suite, so each ladder point is run by the point script
`scripts/moe-128k-point.sh <slug> <N>`, which one **igpu point job**
invokes in a single Bash call (the slimmed iGPU 3B round only has to run
the script and read the SUMMARY line — see the plan's Driver correction
#2). Never from a local brain round.

## Components

| File | What it measures |
|---|---|
| `exercises/0{1..5}_*/` | Coding battery: task.md (contract), tests.py (grader), reference.py (known-good solution, 86 checks total) |
| `run_exercises.py` | Coding battery — one generation per exercise, graded by its tests |
| `run_standard.py` | Standard 3-run prompt sweep (same measurement as the VRAM sweeps) |
| `needle_128k.py` | 128K-context needle — proves the whole window is served, per position |

## Coding battery

Five exercises, each `task.md` + `tests.py` + `reference.py`:

| # | Exercise | Checks |
|---|---|---|
| 01 | LRU cache (`LRUCache`, `get`/`put`, O(1) both) | 8 |
| 02 | Merge overlapping intervals | 8 |
| 03 | Topological sort (Kahn, lexicographically smallest valid order) | 9 |
| 04 | s-expression parser + arithmetic evaluator | 25 |
| 05 | Longest increasing subsequence (length, O(n log n)) | 36 |

Grading: the model gets the task contract plus "respond with exactly
one fenced ```python block for `solution.py`"; the runner extracts the
first fence, writes it as `solution.py` next to the exercise's
`tests.py` in a temp dir, and runs the tests (120 s timeout). The
reference solutions are the known-good baseline — all 86 checks pass
against them, so a failed check is a model error, not a harness error.

```bash
python3 run_exercises.py --base-url http://127.0.0.1:11436/v1 \
    --out results.json [--max-tokens 2048] [--temp 0.2]
```

## Standard 3-run prompt sweep

Same method as the VRAM sweeps: `docs/benchmarks/moe-sweep-prompt.txt`
(~1.5K tokens), three runs; run 1 is cold. Each run makes a
`max_tokens=1` call (bounds prefill) then the real `max_tokens=256`
call; generation tok/s = 256 / (total − prefill).

```bash
python3 run_standard.py --base-url http://127.0.0.1:11436/v1 \
    --prompt docs/benchmarks/moe-sweep-prompt.txt --out standard.json
```

Prints markdown table rows for the results doc.

## 128K needle

Builds a deterministic ~120K-token document (calibrated against the
endpoint's own tokenizer — one and ten filler paragraphs, usage delta
gives tokens/paragraph), inserts `THE NEEDLE CODE IS: <code>` at
0/25/50/75/100% of the way through, and asks the model to quote the
line. Score = exact containment of the code.

```bash
python3 needle_128k.py --base-url http://127.0.0.1:11436/v1 \
    --out needle.json [--target-tokens 120000]
```

A position that 500s or times out is recorded as `found: false` with the
error — that is a real result (the window is not fully served at this
cache size).

## Running the suite at one cache point

`scripts/moe-128k-point.sh <slug> <N>` does a whole point: it stops the
brain, starts `ft serve` on :11436 with the calibrated `--memory-ratio`
(one correction pass if the first free-VRAM reading misses the target by
> 40 MB), runs the three runners into
`~/.local/share/jarvis/bench/moe-128k/<slug>-<N>/`, restores + verifies
the brain, and transcribes the JSONs into the point's results doc
(`docs/benchmarks/moe-128k-<slug>-<N>mb.md`, also copied to the bench
dir). One igpu point job runs the script per ladder point; a follow-up
local round (`moe128k-summary`) adds the numbers to the dashboard's
`/benchmarks` page (`internal/dashboard/moebench.go` — a static report,
numbers are transcribed into Go, not read from files).

The runners can also be driven manually against an already-running box
(the pre-flight smoke test did exactly this against :11435):

```bash
cd bench/coding
python3 run_standard.py --base-url http://127.0.0.1:11436/v1 --prompt docs/benchmarks/moe-sweep-prompt.txt --out standard.json
python3 run_exercises.py --base-url http://127.0.0.1:11436/v1 --out exercises.json
python3 needle_128k.py --base-url http://127.0.0.1:11436/v1 --out needle.json
```

## Notes

- Never point these at port 11434 (the brain) or 11435 (the iGPU
  helper) — the suite is for the `ft serve` box only.
- `run_exercises.py` and `needle_128k.py` pick the model id from
  `/models` when `--model` is not given; `ft serve` advertises the
  served model under its GGUF name.
- Exercises are intentionally small (≤ ~2K-token solutions): the battery
  measures *can it write correct code at 128K context*, not throughput.
- Smoke-tested 2026-11-20 against the iGPU endpoint (:11435,
  Qwen2.5-3B) to prove the runners work end-to-end before the first
  4090 window is spent — that is the one sanctioned exception to the
  "never 11435" rule above; the results are not suite numbers
  (`~/.local/share/jarvis/bench/moe-128k/_smoke-igpu/`).
