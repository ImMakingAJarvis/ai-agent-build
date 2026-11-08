# Brain swap — the photo-window runbook

How the Jarvis brain (27B, llama.cpp, `127.0.0.1:11434`) gets out of the
way of photo generation, and comes back picking up right where it left
off. Tooling: `scripts/jarvis-swap` (installed at `/usr/local/bin/
jarvis-swap`). A short version of the protocol lives in the agent's
CLAUDE.md ("Brain swap" section) so the brain always knows what to do
without re-planning. This doc is the why, the mechanics, and the failure
modes.

## Why this exists

The 4090 has 24.5 GB. The brain holds ~17.6 GB of it. The art service
(Z-Image-Turbo, `127.0.0.1:8120`) and the t2i bench
(`127.0.0.1:8121`) refuse to generate until **12 GB is free**
(`min_free_mb: 12000`, `vram_wait_s: 90` on `/health`). So photo work can
only run while the brain is NOT loaded — and the brain can't just wait:
something has to stop it, watch the window, and start it back. `jarvis-
swap` is that something; the watchdog's maintenance mode is the failsafe.

## The pieces

- **Brain** = the docker compose container `jarvis-engine` (compose
  service `engine`), serving `127.0.0.1:11434` — the same container the
  watchdog restarts (`BRAIN_START_CMD`). It is the ONLY brain deployment:
  the old host `jarvis-engine` systemd unit was removed 2026-11-06 (PR
  #184), so every brain stop/start in scripts and runbooks goes through
  `docker stop|start jarvis-engine` — never `systemctl`.
- **iGPU helper** = Qwen2.5-3B on `127.0.0.1:11435` (container
  `jarvis-engine-igpu`, compose service `engine-igpu`). Jobs with
  `driver: igpu` in their spec run on
  it — `scripts/agent-run` reads the driver from the job spec (32K
  context). Chat already falls back to it when the brain is down
  (`FALLBACK_ENGINE_URL=http://host.docker.internal:11435`). **Photo
  jobs no longer run on it**: they run as code (the `jarvis-photo`
  executor, no model round at all), and their `driver: igpu` spec is
  vestigial band bookkeeping.
- **Photo executor** = `jarvis-photo make --job <id>` (`cmd/jarvis-photo`),
  run by the agentjob Runner for photo-tagged jobs (no model round). It
  is self-sufficient: it starts the art service if it is down, waits for
  the model to load, generates (the PNG is saved to disk and in the
  gallery at the same time), delivers to Discord, and queues the job's
  optional follow-up (`photos/<id>.followup`) as a new job with the
  image path.
- **Watchdog maintenance mode** = `jarvis-watchdog maintain <dur>
  [reason]` (45s|30m|2h|seconds). While unexpired, brain restarts are
  skipped; **at expiry the brain is re-armed even if the whole window
  failed**. That is the hard failsafe: the brain can never be stranded
  off.
- **`jarvis-jobs next` skips jobs whose engine is down**: a ready job
  whose driver's engine does not answer `/health` 200 is skipped (left
  ready) and the next is tried. During a window the worker therefore
  idles through local jobs and runs only igpu/sonnet/superagent ones —
  no doomed round, no autopause, no wasted window.

## The protocol

**The bracket is automatic** (the queue watcher's photo window,
`scripts/agent-run`, docs/agent-run.md): when the loop pulls a job
tagged `photo` it opens the window itself for the whole ready batch
(`jarvis-jobs photo-ready` lists it), and closes it — brain back — as
soon as the batch drains. Photo jobs get the tag from image-queue
ingestion, or by hand: `jarvis-jobs add ... -tags photo`. The brain's
part is now only:

1. `jarvis-swap handoff` — scaffolds `notes/brain-handoff.md`. Fill in
   "In flight" and "First thing on return" (two or three lines each; do
   NOT re-plan — the point is to resume, not restart thinking).
2. Make sure the photo work is queued (image-queue ingestion, or
   `jarvis-jobs add` with `-tags photo -prompt "..."` — the prompt is
   stored in the job's prompt sidecar that the executor reads).
3. End the round. The watcher opens the window on the next pull.

Then the loop carries it (no brain required):

4. First photo pull → `jarvis-swap out` opens the watchdog window
   FIRST, stops the brain container, waits for the VRAM to clear,
   `docker compose up -d art bench` (no-ops if already up), waits for
   the art model to load, then flips the whole ready batch to `igpu`
   at priority 1000 (band bookkeeping — the driver label is vestigial
   for photo jobs, which run as code) and notifies Discord.
5. The photo jobs run one by one as code (the `jarvis-photo` executor —
   no model round, not even the iGPU helper) while the brain is off
   (expected — that is the whole point). Each pull re-flips the batch,
   so photo jobs that arrive mid-window are picked up too.
6. When the batch drains, the loop runs `jarvis-swap in`: closes the
   window, stops art+bench, starts the brain container, waits up to 15 min
   for `/health`, runs the API check (a real completion must come back,
   not just a 200), notifies Discord.
7. Next brain round: read `notes/brain-handoff.md`, do "First thing on
   return", verify the photo results, then `mkdir -p notes/archive &&
   mv notes/brain-handoff.md notes/archive/`, and record `jarvis-notes`.
   Done.

### Manual bracket (fallback)

When agent-run is stopped, or for a one-off window from a terminal, the
original manual bracket still works: from a brain round, enqueue two
fix jobs, both `-driver igpu -priority 100` — **swap-out** (instructions:
"Brain swap OUT. Run exactly `jarvis-swap out --jobs <photo ids>
--maintain 4h --job <this job's id>`. Then `jarvis-swap status`. If it
exited 0, finish the job (exit 0). If not, `jarvis-discord notify 'swap
OUT failed: <last line of ~/.local/state/jarvis/swap.log>'` and finish
with a failure. Do nothing else.") and **swap-in** (same but
`jarvis-swap in`, plus `-depends <every photo job id>`). A manual window
that has a photo batch is adopted by the watcher — the first photo pull
re-flips it and takes over the close — so a manual open is never a dead
end. A manual window with no photo jobs is left to the owner.

## Failure modes — all bounded, none lose the brain

- **A photo job fails** (keepgoing off → the worker autopauses): the
  batch drains, so the loop closes the window on the next iteration —
  there is no swap-in job to strand. Fix or requeue the photo job; the
  next photo pull opens a fresh window. (Manual bracket: the swap-in
  job is stranded — `jarvis-jobs skip <swap-in id>`, then a fresh
  window.)
- **The photo run fails** (art service down or the model never loads
  within the executor's wait): the job fails → autopause → the batch
  drains and the deadline re-arms the brain. Same recovery as a failed
  photo job.
- **The iGPU helper dies mid-window** (manual bracket only: the
  swap-out/swap-in jobs are real igpu model rounds): the igpu round
  fails → autopause → the deadline re-arms the brain. Same recovery.
- **A local job is queued mid-window**: `jarvis-jobs next` skips it
  (brain down); it runs after the window. Nothing to do.
- **The window expires before the photos finish**: the watchdog re-arms
  the brain; the loop's next photo pull opens a fresh window
  automatically (`out` runs again). Manual bracket: art+bench are still
  loaded but gated (below 12 GB free), the flipped jobs are still igpu
  at priority 1000 — run a fresh window, or
  `jarvis-jobs requeue -driver local <id>` the remaining ones (the
  coding parts run on the brain; generation waits for the next window).
- **The brain is down with NO maintenance window**: that is the
  2026-11-04 failure mode, not a swap. `jarvis-swap out` refuses to
  touch it and leaves it to the watchdog.

## Invariants

- **Never stop the brain mid-round.** The watcher runs `jarvis-swap
  out` *between* rounds — before `jarvis-jobs run` — and the manual
  swap-out job runs on the iGPU — so at the moment the brain stops, no
  brain round is in flight. `jarvis-swap out` enforces this
  mechanically: it aborts if a round other than its own is running, and
  a run with no round at all needs `--force`.
- **Never run `jarvis-swap out`/`in` from a local (brain) round** — that
  would stop the engine out from under the running session. Only from
  the queue watcher (between rounds), from an igpu round, or by hand
  from a terminal with `--force`.
- **Idempotent re-runs**: `out` on an already-open window re-flips the
  jobs and resets a fresh maintain; `in` with the brain already serving
  just closes the window.
- The maintain window is opened BEFORE the brain stops, and its expiry
  is the last-resort re-arm — the only reason a mid-swap death cannot
  strand the brain.
- `jarvis-swap status` = one screen of everything (brain container, VRAM,
  art, bench, maintenance, handoff age, running jobs). Log:
  `~/.local/state/jarvis/swap.log`.
