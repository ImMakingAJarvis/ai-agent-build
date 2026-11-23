---
id: task-runner
name: Task runner (run_task loop)
summary: Executes Ready Plane cards from the location queues — worktree, Aider or capability call, acceptance check, merge, push, epic draft PR, board update.
group: board
kind: host-service
status: active
units: [task-runner.service]
binaries: [cmd/task-runner]
ports: ["— (no listener)"]
docs: [docs/plane-to-queue.md, docs/work-board/PLAN.md]
depends_on:
  - to: rabbitmq
    why: "consumes jobs.gpu, jobs.cpu, jobs.igpu; submits cap.* for capability cards"
  - to: plane
    why: "card states and comments (bot)"
  - to: engine
    why: "executor model for LLM cards"
  - to: github
    why: "pushes the epic branch; opens its draft PR and marks it ready (gh + ghapp-token)"
---

## Purpose

Phase C of the work-board plan. Consumes the location queues and drives
each card to Done or Blocked: branch + worktree, an Aider round with the
card's model, the acceptance check, commit, `--no-ff` merge into the epic
branch, a NOTES.md line, a push of the epic branch, board comments. After
the first pushed task it opens a draft PR `epics/<code>` → `main` (linked
on the epic); every later task is one more pushed commit on it, and when
the last task is Done the PR is marked ready for review. A failed push is
commented on the card. A card with `capability:` is
submitted to `cap.<capability>` instead of an LLM round. While a game
runs it pauses `jobs.gpu`.

## Interfaces

- Consumes `jobs.gpu`, `jobs.cpu`, `jobs.igpu`; emits `job.run_task.*`
  events.

## Configuration

- `EnvironmentFile: ~/.config/jarvis/agent.env` — `RABBITMQ_URL`, Plane
  bot credentials, `TASK_RUNNER_REPO`, `TASK_RUNNER_MODELS_FILE`,
  `JARVIS_DATA_DIR`, `STEAM_GAME_EXCLUDES`, `TASK_RUNNER_PRS` (`off`
  disables the epic PR; default on).
- Needs `gh` and `ghapp-token` on the PATH of the `agent` user.
- Runs on the host on purpose: it needs git, the Aider venv and the Go
  toolchain.

## Data

- Worktrees under the repo checkout; job store entries.

## Deploy

    go build -o /tmp/task-runner ./cmd/task-runner && sudo install -m 755 /tmp/task-runner /usr/local/bin/task-runner && sudo systemctl restart task-runner

## Verify live

    systemctl is-active task-runner
    journalctl -u task-runner -n 20 --no-pager

## Failure modes

- Acceptance checks ran in `/` (no `cmd.Dir`), re-runs of Blocked cards
  died on `git worktree add` (both fixed 2026-11-08, EVB-3).
- Location workers died silently after a broker restart (fixed
  2026-11-06, PR #173).
- Until 2026-11-19 the runner never opened a PR (C3 was unbuilt) and a
  failed push was only logged, so finished epics sat on the box.
- The game detector matched the round's own prompt text and paused the GPU
  lane in a loop (2026-11-19, PR #268).
