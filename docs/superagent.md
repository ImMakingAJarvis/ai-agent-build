# Super Agent

A small autonomous agent that takes a goal and works it to completion:
plan the steps, execute them with tools, verify the result, and report —
without per-step human input. It is the native Go counterpart of the
Claude Code harness (`scripts/agent-local`) and is modelled on the
features of DeepSeek's super agent.

- Package: `internal/superagent`
- CLI: `cmd/superagent` (installed at `/usr/local/bin/superagent`)
- Queue driver: `superagent` (see "Queue integration" below)

## DeepSeek super agent → here

| DeepSeek super agent feature | Where it lives here |
|---|---|
| Autonomous goal execution (no per-step prompting) | `Agent.Run`: plan → execute steps → verify, one shot |
| Plans before acting | `plan()`: one model call returns `{"steps":[…],"done_criteria"}` |
| Tool use (shell, files, search) | `Tools`: `shell`, `read_file`, `write_file`, `list_dir`, `memory_search`, `web_fetch` |
| Self-correction | observations feed back to the model; bounded `replan` (default 2) re-runs the planner |
| Verifies before claiming done | `step_done` requires a verification note; a separate `verify()` call checks the goal after the plan runs out |
| Bounded autonomy (no runaway) | budgets: max steps (20), model calls per step (8), run timeout (30 m) |
| Sandboxed / guardrailed tools | `Guard`: blocked command patterns + path confinement (NoBrakes fail-safe style) |
| Progress reporting | every plan step and tool call is logged; the queue Runner tees it to the transcript and the live stream |

## The loop

```
goal → plan (≤8 steps)
       → for each step: bounded turn loop (≤8 model calls)
            model replies end with exactly one <ACTION>{"tool":…,"args":…}</ACTION>
            tool runs (guarded) → observation (truncated to 4000 chars) → next turn
            step_done / replan / finish end the step
       → verify: one model call answers {"ok":bool,"summary":…} against the done criteria
       → Result{OK, Summary, Plan, StepNotes, Turns, Steps, Duration}
```

- The model is a local brain (default `jarvis-brain`) over the engine's
  OpenAI-compatible `/v1/chat/completions` endpoint (`models.Client`,
  thinking disabled).
- A reply without a valid ACTION block gets one nudge ("reply again,
  ending with one ACTION block"); the loop is still bounded by turns.
- `finish` (the model decides the goal is done or impossible) short-circuits
  the loop; `replan` re-runs the planner with the reason and the notes so
  far.
- Exit code of the CLI: **0 iff the agent completed the goal** — so the
  queue Runner's existing "exit 0 = done" rule applies unchanged.

## Tools

| Tool | Args | Notes |
|---|---|---|
| `shell` | `{"cmd": "…", "timeout": 120}` | runs in the work dir; default timeout 120 s, max 600 s; blocked commands return a `BLOCKED:` observation |
| `read_file` | `{"path": "…", "offset": 1, "limit": 200}` | 1-based line window; max 400 lines |
| `write_file` | `{"path": "…", "content": "…"}` | creates parent dirs; confined to the work dir |
| `list_dir` | `{"path": "."}` | sorted, dirs marked with `/` |
| `memory_search` | `{"query": "…", "k": 5}` | jarvis-memory `POST /search` (default `http://localhost:8101`); unreachable → observation, not an error |
| `web_fetch` | `{"url": "…"}` | http/https only; returns status line + first 3000 chars |
| `step_done` | `{"note": "…"}` | the step is achieved; note how it was verified |
| `replan` | `{"reason": "…"}` | the plan no longer fits |
| `finish` | `{"summary": "…", "ok": true}` | the goal is done (or failed) |

## Safety (the Guard)

The same fail-safe philosophy as NoBrakes (`docs/nobrakes/safety.md`):
anything the guard blocks is reported back to the model as a `BLOCKED:`
observation so it can adapt, and the agent degrades to "stuck, try
another way" — never to "did something destructive".

- **Blocked commands**: reboot/shutdown/halt/poweroff, `agent-reboot`,
  mkfs/fdisk/parted/wipefs, `dd … of=/dev/…`, redirects to raw disks
  (`> /dev/sd*`), fork bombs, `sudo rm|mkfs|dd|reboot|shutdown`.
- **Recursive rm of an absolute path outside the work dir** is blocked
  (the classic `rm -rf /home` accident); relative paths and anything
  under the work dir stay allowed.
- **No pushing to main/master** from the agent's shell (repo rule).
- **Path confinement**: file-tool paths resolve against the work dir and
  must stay inside it (plus any `ExtraDirs`); `..` escapes are blocked.
- `--no-guard` disables all of it (not recommended; the queue driver
  never passes it).

## Running it

```
superagent [goal…]                        # one-shot goal
superagent --prompt-file goal.txt         # goal from a file
superagent --job <job-id>                 # goal = the queued job's title + instructions
```

Flags: `--model` (env `LOCAL_MODEL`, default `jarvis-brain`), `--engine`
(env `ENGINE_URL`/`OLLAMA_URL`, default `http://localhost:11434`),
`--workdir` (default: CWD), `--max-steps`, `--max-turns`, `--replans`,
`--run-timeout`, `--memory-url`, `--no-guard`.

The plan, each step, every tool call and the final result print to
stdout; exit 0 iff the goal was completed.

## Queue integration

`superagent` is a first-class job driver, like `local`, `igpu`,
`sonnet`:

- `internal/agentjob`: `ModelFor("superagent")` → env `SUPERAGENT_MODEL`
  → `LOCAL_MODEL` → `jarvis-brain`; `defaultCommand("superagent", …)`
  runs `superagent <round prompt>` (the round prompt = stage-3 brief +
  job text, exactly like the other drivers). The Runner tees its stdout
  to `transcript.txt` and the Redis live stream, and exit 0 = done.
- `jarvis-jobs`: `-driver superagent` is accepted on `run` and `add`,
  and `next`/`show` report it.
- `scripts/agent-run` (the round loop):
  - full auto (`fullauto on`) honours a job's `driver: superagent`
    instead of normalising it to local;
  - the supervised "Who runs it?" prompt offers **Super agent**;
  - a reboot mid-superagent-round re-queues the job for a fresh run
    (there is no Claude conversation to resume — the round is one
    self-contained command).

So: `jarvis-jobs add -id X -title T -driver superagent …`, and the next
round (supervised or full-auto) runs it.

## Tests

```
go test ./internal/superagent/ ./internal/agentjob/ ./cmd/jarvis-jobs/
```

- `plan_test.go`: plan JSON parsing (clean, fenced, garbage, defaults,
  step-count limits).
- `action_test.go`: ACTION-block parsing (clean, last-block-wins,
  missing block, bad JSON, missing tool), arg helpers.
- `guard_test.go`: every blocked pattern, the allowed cases (relative
  rm, in-dir rm, feature-branch pushes), path confinement incl. `..`
  escapes and extra dirs.
- `tools_test.go`: shell (exit codes, work dir, timeout, BLOCKED),
  write/read/list windows, escape attempts, memory_search (live
  httptest server + unreachable degradation), web_fetch, truncation.
- `agent_test.go`: the full loop with a scripted fake model — happy
  path (plan → act → step_done → finish), replan, step-budget
  exhaustion, verifier veto, the no-ACTION nudge, finish-fail,
  turn-budget exhaustion.
- `agentjob_test.go`: the superagent driver's command + env + model
  selection.

## Live verification (2026-11-04, RTX 4090)

Ran on the local brain (`jarvis-brain` on `jarvis-engine`, the second of
the engine's two parallel slots) in a scratch work dir:

1. `superagent "Create a file called hello.txt … containing exactly the
   text 'jarvis super agent'. Then read the file back and check …"` →
   planned 2 steps, wrote the file, verified it with
   `cat` + a shell equality test (`MATCH`), finished `ok=true`.
   **DONE in 15 s, 3 model calls, exit 0.** `/tmp/superagent-live/hello.txt`
   contains exactly `jarvis super agent`.
2. `superagent --job t-sajob` (a job added to a scratch job store) →
   planned 1 step, `echo test > t.txt`, read it back, `step_done` with a
   verification note, finished `ok=true`. **DONE in 12 s, 4 model calls,
   exit 0.** The file contains `test`.
