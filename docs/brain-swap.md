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

- **Brain** = systemd unit `jarvis-engine`, serving `127.0.0.1:11434`
  (the same unit the watchdog restarts; `BRAIN_START_CMD`). The compose
  `engine` container is NOT this: it is chat's slow secondary brain
  (`ENGINE_URL=http://engine:11434`, ~3 GB VRAM + mostly CPU) and it is
  left running through the window — chat keeps working through it, and
  its 3 GB still leaves ~20 GB free after the unit stops, so the art
  gate passes. (Reconciling the two deployments is a separate job.)
- **iGPU helper** = Qwen2.5-3B on `127.0.0.1:11435` (unit
  `jarvis-engine-igpu`). Jobs with `driver: igpu` in their spec run on
  it — `scripts/agent-run` reads the driver from the job spec (32K
  context). Chat already falls back to it when the brain is down
  (`FALLBACK_ENGINE_URL=http://host.docker.internal:11435`).
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

What the brain does, in its own round, when photo work needs the GPU:

1. `jarvis-swap handoff` — scaffolds `notes/brain-handoff.md`. Fill in
   "In flight" and "First thing on return" (two or three lines each; do
   NOT re-plan — the point is to resume, not restart thinking).
2. Enqueue two fix jobs, both `-driver igpu`, `-priority 100`:
   - **swap-out** — instructions: "Brain swap OUT. Run exactly
     `jarvis-swap out --jobs <photo ids> --maintain 4h --job <this
     job's id>`. Then `jarvis-swap status`. If it exited 0, finish the
     job (exit 0). If not, `jarvis-discord notify 'swap OUT failed:
     <last line of ~/.local/state/jarvis/swap.log>'` and finish with a
     failure. Do nothing else."
   - **swap-in** — same but `jarvis-swap in`, plus
     `-depends <every photo job id>`.
   `<photo ids>` = the queued photo/t2i jobs this window exists to
   unblock.
3. End the round. The brain is no longer involved.

Then the worker carries it (no brain required):

4. swap-out runs on the 3B → `jarvis-swap out` opens the 4h watchdog
   window FIRST, stops the brain unit, waits for the VRAM to clear,
   `docker compose up -d art bench` (no-ops if already up), waits for
   the art model to load, then flips the photo jobs to `igpu` at
   priority 1000 and notifies Discord.
5. The photo jobs run one by one on the iGPU helper while the brain is
   off (expected — that is the whole point).
6. When all the photo jobs are done, swap-in's deps are met and it runs
   on the 3B → `jarvis-swap in`: closes the window, stops art+bench,
   starts the brain unit, waits up to 15 min for `/health`, runs the API
   check (a real completion must come back, not just a 200), notifies
   Discord.
7. Next brain round: read `notes/brain-handoff.md`, do "First thing on
   return", verify the photo results, then `mkdir -p notes/archive &&
   mv notes/brain-handoff.md notes/archive/`, and record `jarvis-notes`.
   Done.

## Failure modes — all bounded, none lose the brain

- **A photo job fails** (keepgoing off → the worker autopauses): the
  window stays open; the watchdog re-arms the brain at the deadline. On
  return: the swap-in job is stranded (its deps never completed) —
  `jarvis-jobs skip <swap-in id>`, fix or requeue the photo job, run a
  fresh window.
- **The iGPU helper dies mid-window**: the igpu round fails →
  autopause → the deadline re-arms the brain. Same recovery.
- **A local job is queued mid-window**: `jarvis-jobs next` skips it
  (brain down); it runs after the window. Nothing to do.
- **The window expires before the photos finish**: the brain is back and
  art+bench are still loaded but gated (below 12 GB free); the flipped
  jobs are still igpu at priority 1000. On return, decide: run a fresh
  window, or `jarvis-jobs requeue -driver local <id>` the remaining ones
  (the coding parts run on the brain; generation waits for the next
  window).
- **The brain is down with NO maintenance window**: that is the
  2026-11-04 failure mode, not a swap. `jarvis-swap out` refuses to
  touch it and leaves it to the watchdog.

## Invariants

- **Never stop the brain mid-round.** The swap-out job is a round of its
  own and it runs on the iGPU — so at the moment the brain stops, no
  brain round is in flight. `jarvis-swap out` enforces this
  mechanically: it aborts if a round other than its own is running, and
  a manual run (no round at all) needs `--force`.
- **Never run `jarvis-swap out`/`in` from a local (brain) round** — that
  would stop the engine out from under the running session. Only from an
  igpu round, or by hand from a terminal with `--force`.
- **Idempotent re-runs**: `out` on an already-open window re-flips the
  jobs and resets a fresh maintain; `in` with the brain already serving
  just closes the window.
- The maintain window is opened BEFORE the brain stops, and its expiry
  is the last-resort re-arm — the only reason a mid-swap death cannot
  strand the brain.
- `jarvis-swap status` = one screen of everything (brain unit, VRAM,
  art, bench, maintenance, handoff age, running jobs). Log:
  `~/.local/state/jarvis/swap.log`.
