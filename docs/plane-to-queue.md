# Plane → Queue: how board work reaches the agent

Step-by-step guide for getting work from the Plane board
(`http://111.1.2.222:8088`, workspace `jarvis`) into the job queue and
out the other end as merged code. One project per epic (E2E, TST, NBR,
MOE, QDB, VCH, T2I, WBD, GLM — see `docs/work-board/NOTES.md`).

## The short answer to "do I need to do anything to trigger it?"

**No, once the epic is planned and approved.** The only states and
labels that matter:

| You do this on the board | What happens |
| --- | --- |
| Epic in **Backlog** gets a `plan:<model>` label | A `plan_epic` job is queued at the front of the background queue; the epic moves to **Planning** |
| You move the epic to **Plan approved** | Every sub-item moves to **Ready** and each one is queued as a `run_task` job. Nothing else to do — tasks run one at a time in dependency order |
| You move **one task card** to **Ready** | That single card is queued as a `run_task` job immediately (this is the manual single-task path) |
| You move an epic back to **Planning** with a comment | Re-plan: the comments are folded into the new planning prompt |

There is no "start" button and no per-task tagging. A card in **Ready**
*is* the trigger. A card in Backlog/Plan review does nothing until it
reaches Ready (by approval or by hand).

## Labels, and which ones are triggers

- `epic` — marks the parent item. Planning only fires on epics.
- `plan:<model id>` — **the planning trigger**, and the model that writes
  the plan (e.g. `plan:claude-sonnet-4-5`). Add it to an epic that is in
  Backlog. The labels are kept in sync with `config/models.yaml`
  (enabled models with the `planner` role only).
- `model:<model id>` — the **executor** model for a task card. The
  planner sets this on every task it creates; you can change it on the
  board before approval. Placement (which queue it runs on) follows the
  registry's `home_location` for that model.
- `run:gpu` / `run:cpu` / `run:igpu` — optional **placement override**.
  Add one when you want a task somewhere other than its model's home
  (e.g. force CPU to keep the 4090 free). An override that can't work
  (the 27B on `run:igpu`) moves the card to **Blocked** with a comment
  explaining why.
- `effort:low` / `effort:medium` — optional, planning depth. Default high.

## The full lifecycle of one epic

1. **Create the epic.** `plane-epic create "<title>" --identifier XXX`
   scaffolds the project (states, labels, editors), creates the epic
   work item, and registers the project with the bridge. Add the epic's
   description there — it is the planner's brief.
2. **Kick off planning.** Add `plan:<model>` to the epic while it is in
   Backlog. The bridge queues `plan_epic`, the epic moves to Planning,
   and you get a comment with its queue position.
3. **Plan lands.** The chosen model runs Plan → Decompose → Review
   (fresh context each). Output: a Plane **page** with the plan,
   linked from the epic, plus one **sub-item per task** in **Plan
   review** — each with a description, an acceptance check, `blocked by`
   relations, and a `model:` label. The epic moves to Plan review.
4. **Approve.** Edit, delete or reorder the sub-items if you want, then
   move the epic to **Plan approved**. All sub-items go to Ready and the
   execution starts. To change your mind later, comment on the epic and
   move it back to Planning (re-plan).
5. **Execution.** `task-runner` (the host service) consumes the location
   queues (`jobs.gpu` / `jobs.cpu` / `jobs.igpu`). Per card: worktree +
   branch `epic-<code>/task-<n>`, context build (plan page, NOTES.md,
   the card), the executor model edits via the Aider harness, then the
   card's acceptance check plus `go build` / `go vet`. Up to 3 attempts
   with the errors fed back.
   - **Pass** → commit, merge into the epic branch, card → **Done**, a
     line in `docs/work-board/NOTES.md`.
   - **Fail 3×** → card → **Blocked** with a comment. (The Discord ping
     for this path is C2, next in the queue.)
   - **Dependency not Done** → the card waits; it runs as soon as its
     blockers are Done.
6. **Epic done → PR to main.** When every sub-item is Done, one PR from
   the epic branch is opened and linked on the epic (C3, also queued
   after C2). Until that lands, the epic branch is the unit of review.

## Running a single task by hand (no planning)

Create a sub-item under any epic in the Plane UI (or have the planner do
it), give it:

- a `model:<id>` label (which model executes it),
- a description that ends with `Acceptance check: <command>`,

then **move it to Ready**. That is the entire trigger. If the model
label is missing or unknown, the card goes to Blocked with a comment
instead of being queued.

## What the queue actually is

- `plane-bridge` (systemd) receives Plane webhooks **and** runs a
  catch-up sync on every start, so a card moved while the box or the
  bridge was down still gets queued on the next start/restart.
- Job ids are deterministic (`plane-run_task-<item id>`), so a card
  never gets two jobs. A job that ends **failed/skipped** while its card
  is still Ready is re-triggered on the next catch-up — a dead broker
  publish self-heals. An in-flight job owns its card; repeated triggers
  are no-ops.
- Placement: the `model:` label → registry `home_location` (with
  fallback), overridable by a `run:` label. GPU-paused-by-game-watchdog
  is honored — the job waits in its queue lane, not on the board.

## Useful CLI

```
plane-epic create "<title>" --identifier XXX [--description "..."]
jarvis-board item <ref>            # one-line state of a card
jarvis-board comment <ref> "..."   # model-prefixed comment
jarvis-board state <ref> <State>   # move a card (flags before args)
jarvis-board done <ref> "..."      # Done + comment
jarvis-board link <ref>            # browse URL
```

## The current holdup (2026-11-06)

- **C1 — the execution loop itself — is this PR.** The code is built
  and the `task-runner` service is deployed and running; it just needs
  the branch merged. Until then, Ready cards queue but the merge path
  (epic branch → main) and the board state write-back are not in main.
- **C2 (Blocked → Discord ping), C3 (epic PR to main), C4 (per-job
  metrics)** are planned and already in the ready queue, as are
  **D1** (pull + benchmark the Flash Next MoE) and **D2**
  (`docs/adding-a-model.md`); they run after this round.
- **TST-3** (the toy "/health endpoint" card that exercises C1
  end-to-end) was stuck on stale git worktrees left by the C1 round;
  those are cleaned up and the job requeued.
- **Backlog epics** (NBR, MOE, VCH, T2I, GLM) hold ~35 tasks that will
  not enter the queue until each epic is approved (or individual cards
  are moved to Ready by hand). That is a go/no-go call, not a bug.
- Several **WBD** cards were left in Backlog although the work merged
  long ago (state write-back predates C1); they are being tidied to
  Done by hand.
