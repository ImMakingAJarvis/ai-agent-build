# dashboard-ops-ideas — five ops improvements: see why it failed, know if the GPU is free, watch the live job, compare bench models, trust the board

2026-11-20, think job. Sources: the live round snapshot (62 failed / 83 ready / 1
running), `internal/dashboard/*.go` (routes, `queueJob`, `machine`, `moebench`,
`kanban` structs), `docs/dashboard.md`, `notes/ideas/failed-triage.md` (11-19).
Note only — no code, no jobs moved.

## What I found

The dashboard is already deep, so the gap is **not missing pages** — it is
*operational friction in what exists*. Pages present: 4-band drag queue
(photo/fix/build/background/failed) with reorder + band priority, a machine
view (VRAM, swap, hold, window, art `/health`), a live job log, `/map` (52
nodes, per-node health), `/benchmarks`, `/kanban` (full CRUD), `/secrets`. The
operator keeps having to **click into a single row to learn the one fact that
decides the next action**. The two loudest numbers in the snapshot — 62 failed
and the constant "can I run a photo/bench *now*?" — are exactly where that
click lives.

Evidence per area (all verified against the code):

- **Queue** — `queueJob` (`queue.go`) has no `Error` field, so the *failed band
  shows title + priority + driver only, not why it died*. The reason already
  exists in the store (`agentjob/store.go:181`) and is even rendered in the
  recent-jobs table (`jobs-section.html:87` renders `.Error`) — the failed band
  just never receives it. `failed-triage.md` was a one-time **manual** pass that
  opened every `state.json` to sort 62 jobs.
- **Engines** — `machine.go` already computes VRAM used/free, swap state, hold,
  window, and probes the art service `/health`. What it lacks is a single
  **verdict**: "is there room for a photo job (~12 GB) right now — yes/no, and
  why if no (brain holds ~18.5 GB / window closed)." The operator reads three
  numbers and does the subtraction by hand.
- **Jobs** — the one *running* job is the whole stack's current activity, but it
  is a row with a "▶ in progress" chip and a Log link. To know **what it is doing
  right now** you must open `/jobs/{id}`. The SSE stream `/api/jobs/{id}/live`
  already exists — it is not surfaced at the top level.
- **Benchmarks** — `/benchmarks` renders per-model tables + bars from
  `docs/benchmarks/*.md` (`moeTable`). Comparing two models, or "current brain
  vs best," is eyeball. No side-by-side, no delta, no "this is the installed
  brain" marker.
- **Kanban** — a task carries `Status`/`Priority`/`branch`/`model` but **no link
  to the agent job or PR that did it**; status is flipped by hand. `t2i -3`
  (failed-triage): the job was **done 11-06 but the kanban status never flipped**
  → the board lies. The dashboard already reads both the kanban store and the
  agentjob store, so joining them is a lookup, not new infra.
- (Bonus, not one of the five: there is **no `/health` route on :8080** — 404
  confirmed, matching failed-triage. `/api/map/health` is the map's node probe,
  not a stack liveness endpoint.)

## The five ideas (one per named area)

1. **Queue — Failed-band triage: reason + signature + bulk actions.** Add
   `Error` to `queueJob`, render the failure reason inline in the failed band
   (data already in the store, already rendered elsewhere), group failed jobs by
   normalized failure signature with counts, and add per-signature *Requeue-all /
   Drop-all* (the dead-letters band already has per-row Requeue/Drop — extend the
   pattern). Turns the 62-job manual triage into a 10-second glance.
   **Highest value** — 62 failed is the loudest number in the snapshot. Effort:
   small–medium. First step: add `Error` to `queueJob` + render it, then group.

2. **Engines — GPU-headroom verdict ("can I run X now?").** One tile on the
   machine view answering the real question: from the already-computed free VRAM,
   is there room for (a) a photo job (~12 GB), (b) a `bench.run` sweep, (c) the
   iGPU helper — each yes/no, with the reason if no. `machine.go` already has all
   the inputs (VRAM, swap, hold, window, art probe); this composes them into one
   green/amber/red verdict. Effort: small. First step: a `gpuCanRun(capability)`
   returning `{photo, bench: yes/no + reason}`, rendered as a tile.

3. **Jobs — "Running now" hero with a live tail.** Promote the single running job
   to a top-of-index card: title, driver, elapsed (since `started_at`), and a
   3-line auto-refreshing tail of the latest events (reuse the existing
   `/api/jobs/{id}/live` stream). A glance answers "what is the brain doing right
   now" without clicking in. A display change, not new plumbing. Effort: small–medium.
   First step: a "Running now" card at the top of the index with the last event line.

4. **Benchmarks — side-by-side comparison + "installed brain" marker.** On
   `/benchmarks`, add a comparison mode: pick two (or all) models, show tok/s +
   VRAM at the same context points in one table with deltas, and badge the
   installed brain (Qwen3.8-27B Q4_K_M) so "current vs best" is one glance. The
   page already parses the per-model tables (`moeTable`); this is a second render
   mode over the same data. Effort: medium. First step: a comparison table for the
   two most-compared models + an "installed" badge.

5. **Kanban — board reflects reality (link task → job/PR, auto-status).** Give
   each task an optional `job_id`/`pr` field; render, per task, the linked job's
   live status (done/failed/running) or the PR's merged/open state; optionally
   suggest flipping the task status when the linked job is done. Fixes the `t2i -3`
   "board lies" case and turns the board from manually-maintained into a live ops
   view. The dashboard already reads both stores — this is a join. Effort: medium.
   First step: add `job_id` to the task model + render the job's status chip.

## Recommendation

Do **#1 first** — cheapest, attacks the loudest number (62 failed), and the data
is already there. **#2** and **#3** are both small and high-glance-value; do them
next. **#4** and **#5** are medium and pay off as the bench and board grow. All
five are display/plumbing over data the stack already holds — **none need a new
service**. If only one ships, #1.

**Considered, not picked:** a `/health` liveness route on :8080 (a real gap, 404
confirmed — but it is a one-line route, better filed as a tiny build job than an
idea; `/api/map/health` already gives per-node health); a global "is the stack
healthy" banner (`/map`'s 16-up/3-down nearly covers it); a queue
ETA/predicted-finish-time view (noisy, low value versus #1).
