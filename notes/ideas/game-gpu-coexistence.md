# game-gpu-coexistence — the game path is fully automatic and live; the real gap is that its policy lives only in code comments

**Bottom line:** when a Steam game launches, the brain yields the 4090 with *no
human in the loop*. `resourced` (the window controller, host systemd, verified
`active` 10 h at this round) detects the game, cuts the in-flight round, stops
the brain container, and frees ~20 GB; when the game ends it brings the brain
back and re-queues the cut round. This is **more** aggressive than the photo swap
window, because it must also handle the case where a game starts *mid-round* —
which the photo path never faces. So "the gap versus the automatic photo swap
window" is not "game is manual, photo is auto." The gaps are narrower: the
policy is undocumented, detection only covers Steam-library games, and the
mid-round handoff is a blunt cut, not a pause.

## What happens today when a Steam game launches (verified in code)

`resourced` runs its own `streamwatcher` (`internal/streamwatcher`, 2 s poll):
a `/proc/<pid>/cmdline` scan for a binary/launch-wrapper inside
`…/steamapps/common/<Game>/` (native, Proton, or Wine), Steam Remote Play
(TCP 27000–27009), Sunshine (UDP 48010 connected), and NVENC encoder utilization
as the always-works fallback. `IsGame()` = `SteamGame` or `SteamRemotePlay`
(the game runs on this host). A **Sunshine stream is NOT a game** — it is plain
desktop streaming, needs only NVENC (~hundreds of MB), and takes no GPU action
(`streamwatcher.go:101`).

On a game-kind `stream_start` (`cmd/resourced/stream.go:254`):
1. **Hold first** — the ledger's GPU hold turns on so a worker re-asking queues,
   not grants into the headroom the game is about to take.
2. **Revoke** every capability worker's GPU lease (they drain their in-flight
   job on their next ~30 s poll; CPU/iGPU leases are never touched).
3. **Game-hold marker** — `~/.local/state/jarvis/game-hold` is written *before*
   the brain stop, so `jarvis-jobs next` skips all driver-local jobs
   (`cmd/jarvis-jobs/main.go:819`). This closes the gap around the window where
   the engine-down skip wouldn't (brain-out pending / brain coming back).
4. **Brain out** — `AcquireForGame` → `jarvis-swap out --game --force`: opens a
   4 h watchdog-maintenance window first, stops the brain container, waits for
   the VRAM to clear. **No art/bench loads** — the game wants that VRAM. Marker
   records `for:["game"]`.

**The mid-round cut** (`stream.go:447` `acquireForGame`) is the mechanism that is
new versus the photo path. If a brain round is in flight, `jarvis-swap`'s round
guard aborts (invariant 1 — the brain never stops itself mid-round). So resourced
**cuts the round**: it writes the `restart-signal` (content `steam-game`), the
runner kills the round within a second, the job goes back to `ready`, and it
re-runs in a fresh conversation after the game. One Discord line tells the owner.
A keepalive (5 s while the brain is up / window not the game's; 60 s steady,
re-armed on every transition) retries the brain-out, re-arms the 4 h maintenance
for long sessions, and converts a photo window into the game's.

On `stream_end` (`stream.go:316`): `ReleaseFromGame` closes the window *only if
it is the game's* (a photo window that opened during the game is left for queue
policy), blocks until the brain is back, then releases the GPU hold and clears
the marker (a 30 s sweeper clears them if the close failed); `ledger.Reevaluate()`
re-grants the queued leases.

**Why the cut exists — two real incidents.** 2026-11-18: a round ran 25 min, a
game 10 min, the round guard blocked the brain-out, and the brain never released
its VRAM (the game got a black stream). 2026-11-19 02:02: the transition never
re-armed the keepalive, so the 5 s brain-out retry was up to 60 s away, the 39 s
session ended first, and the VRAM was never freed. Both fixed (the round-cut +
the re-arm). This is why the yield procedure is "automatic" rather than "ask
first."

## The gap versus the automatic photo swap window

| | Photo window | Game window |
|---|---|---|
| Trigger | a `photo`-tagged job is ready (batch-scoped) | a game process is detected (session-scoped) |
| Closes when | the ready batch drains | the game ends — for its whole life |
| Mid-round | opens *between* rounds; waits for the next tick | can start *mid-round*; **cuts** the round |
| Loads | art + bench | nothing (the game takes the VRAM) |
| Local jobs gated by | engine-down skip | engine-down skip **and** the game-hold marker |

The photo window is a queue policy (EVB-27); the game window is an event
(`EVB-29`, "the game watchdog wins"). Both are owned by `resourced` and both
use `jarvis-swap`, but the game path is the one that has to win against an
in-flight round, and that is where all its complexity (and both incidents) live.

**What is still a gap (the actual findings):**
- **No policy doc.** The whole behavior is documented in `stream.go`'s 80-line
  comment block, the `!igpu`/photo sections of `docs/agent-run.md`, and a comment
  in the unit file. `docs/services.md`'s `resourced` row says nothing about games.
  A fresh brain round (or Nathan) has to dig into Go to learn "launching a game
  is safe and automatic." This note is the seed for a `docs/game-gpu.md`.
- **Detection only sees Steam-library games.** A game outside a
  `steamapps/common/` install (a non-Steam game, a portable exe, a launcher that
  hides the path) is not detected → the brain does not yield → a VRAM fight.
  `STEAM_GAME_EXCLUDES` is an *opt-out*; there is no opt-in list or
  VRAM-pressure fallback for the undetectable case. (`STEAM_GAME_EXCLUDES` is
  currently empty = every Steam game evicts the brain.)
- **The cut is blunt.** A long round is discarded and re-run fresh — the in-round
  context is stale anyway, so the re-run is "correct," but work done that round
  (a PR about to be opened) is not checkpointed; it's redone. There is no
  "pause and resume the same conversation."
- **Single point of failure.** The whole path is `resourced`. It restarts
  mid-game cleanly (boots mid-stream → takes the game path immediately, and
  `sweepBrainBack` clears a stale marker), but it is one process owning the
  hold, the cut, and the window.

## Recommended yield procedure

- **Default — do nothing.** Launch the game. `resourced` detects it (≤2 s), cuts
  any in-flight round, stops the brain, and frees the VRAM. After the game the
  brain is back and the cut round re-runs; one Discord line confirms the cut.
  This is the recommended procedure because it is automatic and self-healing.
  **Never run `jarvis-swap out --game` by hand from a brain round** (invariant 1);
  only resourced (event-driven) or a terminal with `--force` may.
- **Optional — a controlled handoff for a long/important round.** If you are
  mid a round you don't want bluntly cut, park it *before* launching:
  `jarvis-jobs stop <id>` (or `jarvis-jobs yield <id> --wait-on … --in-flight …
  --next …` if it is parked on a result). The job then waits through the game
  (engine-down + game-hold) and resumes in the next round, instead of being
  restarted. Or simply let a near-finished round end — the path never stops a
  round that has already ended.
- **For light games you don't want to evict the brain for:** set
  `STEAM_GAME_EXCLUDES=<comma,separated,cmdline,substrings>` in
  `resourced.service`, `systemctl restart resourced`.

In short: the "yield" the owner performs is conceptual — the round is cut and
re-queued, which *is* the yield. The only manual yield worth doing is the
optional pre-park above, for a controlled handoff.

## Options / next steps

1. **Write `docs/game-gpu.md`** (1 page): the automatic path, the mid-round cut,
   the yield procedure, `STEAM_GAME_EXCLUDES`, and the failure modes. Pull the
   policy out of `stream.go`'s comment block into a doc a new round can find.
   Cheap, high value; this note is the seed.
2. **Add the game path to the `resourced` row in `docs/services.md`** (one line:
   "also runs the game/stream watchdog — a Steam game stops the brain
   (`jarvis-swap out --game`), a Sunshine desktop stream does not").
3. **(Optional) Surface `for:["game"]` on the dashboard** — `swap-window.json`
   already records it; a "brain down for a game" line at a glance removes the
   "why is the brain down?" question.
4. **(Optional) Detection robustness** — an opt-in list, or a VRAM-pressure
   fallback (if free VRAM drops below a floor while an *unclassified* GPU process
   is active, yield anyway) to catch non-Steam / non-standard games.
5. **(Optional, a decision for Nathan) Softer cut** — let a round that is
   mid-PR finish within a short grace before the game preempts it. Trades the
   game's VRAM against a round; probably not worth it since rounds are cheap to
   re-run. Flagging only.

## Recommendation

The game path is **done and live — no build is needed.** The one concrete,
cheap, high-value next step is **#1: write `docs/game-gpu.md`** so the policy
(which currently lives only in code comments) becomes a doc a fresh round can
read, plus the one-line #2 services.md fix. Set `STEAM_GAME_EXCLUDES` only if
Nathan names specific light games that should coexist with the brain. #3–#5 are
optional polish, not gaps in correctness.
