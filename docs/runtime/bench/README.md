# Runtime benchmark (ELR-78)

A fixed benchmark to run before and after each milestone of the Event-Loop
Agent Runtime epic (ELR-1): is the runtime actually better than Claude Code
rounds on the local brain? It depends on nothing in the runtime and runs today.

## What is in it

- `suite.json` — six jobs (same instruction text for every driver), three
  scenarios, and the check command for each job.
- `fixtures/<job>/` — one tiny self-contained directory per job. The script
  copies them to a fresh temp workspace for every run; the fixtures here are
  never edited.
- `../../../scripts/bench_runtime.sh <driver> <scenario> <out.json>` — the runner.
- `../../../scripts/bench_runtime_test.sh` — runs the runner with the `fake`
  driver to prove collection and JSON shape (no model, throwaway job store).

| Job | What the job does | Check |
|---|---|---|
| edit-small | fix one wrong function in a Go module | `go test ./...` |
| slow-test | same, but the test suite takes ~60 s (sleep in TestMain) | `go test ./...` |
| slow-command | run `./fetch.sh` (120 s), write the line count of `data.txt` to `answer.txt` | answer equals `wc -l` |
| multi-file | rename `OldName` to `NewName` across five files | `go build ./... && ! grep -rn OldName .` |
| big-log | `./build.sh` prints ~200 KB with one real error line; fix the cause | `./build.sh` exits 0 |
| docs | write a README `## API` section for the three exported functions | `grep -c` of the three names = 3 |

Scenarios: **S1** each job alone, one after another · **S2** all six queued at
the same moment · **S3** S2 plus five chat messages sent 60 s apart while it runs.

## How to run

    scripts/bench_runtime.sh local S1 /tmp/local-S1-a.json
    scripts/bench_runtime.sh local S2 /tmp/local-S2-a.json
    scripts/bench_runtime.sh local S3 /tmp/local-S3-a.json

Drivers: `local` (brain), `igpu`, `sonnet`, `opus`, and `fake` (no model).
The jobs are created in the real job store with `jarvis-jobs add -tags bench
-class background -driver <driver>` and run by the normal `agent-run` loop, so
**`agent-run` must be running in full-auto** (`jarvis-jobs fullauto on`) or the
jobs wait at the Discord checkpoint. Queue nothing else meanwhile — the numbers
measure a quiet queue. Bench jobs carry the tag `bench` and the id prefix
`bench-<time>-<pid>-<job>`. Allow about 20 min for S1 and 1 h of timeout
(`BENCH_TIMEOUT`, per job); S1 is dominated by the two 60–120 s jobs.
Env knobs are listed at the top of the script (`BENCH_PROM`, `BENCH_ENGINE_URL`,
`BENCH_TEMP`, `BENCH_SEED`, `BENCH_CHECK_TIMEOUT`, `BENCH_KEEP=1` keeps the workspace).

## What each number means (result file)

Per job (`jobs[]`):
- `status` — job store status when the script saw it finish (`done`, `failed`, `timeout`).
- `check_passed` — the fixture's check command, run after the job in its workspace.
  This is the real outcome; `status: done` with `check_passed: false` is a wrong answer.
- `wall_s` — seconds from the job being added to the script seeing it finished
  (queue wait included, so in S2/S3 later jobs wait for earlier ones).
- `kwh` — the job store's sampled energy for that job (local drivers; null for cloud).

Whole run (`run`):
- `wall_s`, `jobs_passed` (of 6, by check), `total_kwh`.
- `gpu.util_avg_pct`, `gpu.vram_peak_mb` — Prometheus (`:9090`) `nvidia_gpu_utilization`
  and `nvidia_gpu_mem_used_mb` over the run, 5 s step. Null when Prometheus is unreachable.
- `gpu.idle_while_unfinished_s` — seconds the GPU was under 10 % busy while at least
  one job was unfinished (5 s resolution). High = the agent waits on something
  (a slow command, a poll loop) instead of the GPU working; the runtime should lower it.
- `tokens.prompt`, `tokens.generated` — difference of the engine's
  `llamacpp:prompt_tokens_total` / `llamacpp:tokens_predicted_total` before and after.
  Cached prompt tokens are not counted (llama.cpp excludes them). Local/igpu drivers only.
- `chat[]` (S3) — per message the offset sent and `reply_s`, the time for a short
  reply (≤120 tokens) from the driver's engine, sent directly to `/v1/chat/completions`
  while the jobs run. It measures how long the chat waits behind the work. Not
  available for `sonnet`/`opus` (`result: no-engine`).

`sampling` records temperature, seed and model file. The seed and temperature are
applied to the chat messages; job rounds use the driver's own sampling (the harness
does not let the script set a seed), so the seed is a request, not a guarantee.

## Noise rule

The model samples randomly, so two runs of the **same** setup differ. For every
number: run each setup at least three times (`-a`, `-b`, `-c`), take the spread
(max − min) of that number, and call a difference between two setups smaller than
that spread **no change** — not better, not worse. Put that in the comparison table:

| number | before (a,b,c) | spread | after (a,b,c) | verdict |
|---|---|---|---|---|
| wall_s S2 | 910, 1010, 980 | 100 | 700, 760, 790 | better (diff > spread) |
| jobs_passed S1 | 5, 6, 5 | 1 | 6, 5, 6 | no change |

Pass/fail counts are especially noisy with six jobs — one flipped job is within noise.

## Self-test

`bash scripts/bench_runtime_test.sh` — S1 and S3 with the `fake` driver; asserts the
JSON shape, that jobs finish `done`, that every check fails (nothing was fixed), and
that S3 records chat messages. Takes about 15 s.
