# stack-ideas-5 — five projects that make the stack earn its place: prove it nightly, finish Reel, house brain, better brain, real voice

2026-11-19, think job (stack-ideas-5). Built from the four sibling notes
written earlier today (render-deep-dive, power-economics, docs-drift-audit,
failed-triage), the live queue (82 ready incl. the think floor, 62 failed),
`notes/agent/asked.md`, the think backlog, and the round snapshot. Deliberately
not re-pitched: GPU-share (no-go → BOINC), the $22/mo power model, docs drift,
and the 62-job failure triage — each has its own note.

## What I found

- **The stack's weakness is verification, not capacity.** 29 containers, two
  healthy engines, 82 jobs ready — but the things that make it *trustable* are
  the unproven ones: `GET /health` on :8080 is still 404 today (checked live),
  the voice-call E2E never ran (PR #263 merged, `voice-call-e2e-1` failed),
  the photo delivery-marker fix has no live test photo, and the agent's
  definition of done carries a `NOT VERIFIED LIVE` escape hatch that jobs use
  regularly. Today's failed-triage is a one-time manual pass at exactly this
  debt — the project below makes the pass continuous.
- **The biggest queued product is one job from unblocking.** Reel -1..-3 done;
  -5..-16 (12 jobs: web UI → direct play → profiles → Android/Apple apps →
  Tailscale → v1 field test) all sit ready, behind the single failed job, the
  library scanner (-4).
- **Nathan's live interests (asked.md, 11-18 when he was home):** benchmark
  results on a website (shipped, PR #260), then "install the model as
  thinker/planner/coder if it's a good coder" with coding exercises in the
  bench — the sweep is ~80% done, the coding half and the install decision
  are not; he tested voice chat that same evening and the standing ask is
  "natural/conversational, not a bad GLaDOS imitation"; the box is the heater
  for his mother's place (idle-work.md) and the power ledger already knows
  its own draw (power-economics.md).

## The five ideas, ranked by value/effort

### 1. Nightly self-proof: an E2E health gate
One scheduled script (not a model round) that actually exercises the major
paths and reports PASS/FAIL into the morning digest and the dashboard:
voice round-trip, photo round-trip in the swap window, a minimal job
queue→run→done round-trip, memory search, HA light on/off + read-back, and
engine + `/health` liveness. It turns "done" from a claim into a measurement
and makes regressions visible the next morning instead of in a confused
thumbs-up. Every probe is small; the harness is one script, a timer, a digest
line. Value: high — every other project here is more trustworthy if its
results are proven, not just tested. Effort: medium-low.
**First step:** the three probes failed-triage flagged, as one verify-batch
job (one test photo through the photo path, re-run `voice-call-e2e-1` with a
slimmer prompt, add the missing `/health` route), then generalize into
`scripts/stack_verify.py` + a systemd timer.

### 2. Finish Reel — the 13-job path to his viewing app
Reel is the only queued product aimed at Nathan's daily life: his Plex clone
on this box (Jellyfin stays the baseline). The plan is fully scoped and 80%
of the queue is in place — web UI, direct play, profiles, Android/Apple apps,
Tailscale remote, v1 field test — and all 12 jobs are ready, waiting on the
single failed library-scanner job (-4). Finishing Reel is the first time this
stack ships a consumer app he actually uses, and Gate 2's parity/fork
decision (-10) is already queued as the built-in stop point. Value: high;
effort: medium and bounded — the design is already paid for, execution
remains. **First step:** requeue the scanner job (failed-triage verified it
is the only blocker) and let the 12 ready jobs run in order; `think-reel-gap`
runs alongside to confirm scope.

### 3. House brain — Jarvis as the home's control plane
The pieces are all present: HA MCP + REST, GE/SmartHQ appliance tools,
jarvis-ha sensors, TTS announcements (EVB-6), and its own per-job power
ledger. What is missing is the connective tissue — automations that make the
house respond: presence-based lights and TTS greeting, the box's heat
scheduled against the house's heating (it is his mother's heater and already
knows its draw), appliance scheduling while the box idles. This is the idea
that changes what the stack *is* for Nathan: from server-room toy to home.
Value: very high for him; effort: medium — the tools exist, automations are
config plus small glue. **First step:** the queued `think-ha-automation-ideas`
think job picks the top five; build the two it ranks highest.

### 4. Upgrade the brain — finish the MoE bench, install the winner
Nathan's own 11-18 ask: find the optimal cache size at 128K context for every
MoE on the bench, results to the benchmark page, and install the model as
thinker/planner/coder if it codes well. The sweep is nearly done (Qwen flash
points in; the GLM tail is missing — failed-triage specifies the exact job),
the bench page exists (PR #260), and `bench.run` runs sweeps without
consuming brain rounds. The missing piece is the coding-exercise benchmark —
the "is it a good coder" half — and the install decision itself. If a local
model wins, this upgrades the agent's core loop; it is the highest-leverage
upgrade available, for medium effort. **First step:** the GLM-tail job
(missing 150/400MB points + summary doc via bench.run), plus one
coding-exercise prompt added to the bench harness.

### 5. Voice that feels like a person
The most recent standing ask (11-12, carried from 10-27): "make the voice
sound natural/conversational, not a bad GLaDOS imitation" — and he tested
voice chat on 11-18. But the path is unmeasured: the E2E never ran, no one
has mapped where the latency lives, and his Piper access-path decision is
still queued from when he was away. This is the feature that makes Jarvis a
presence in the house rather than a dashboard; audio latency is also the
hardest kind of engineering on this box, so it ranks last on value/effort
despite high value. **First step:** the queued `think-voice-latency-map`
think job (maps the stages, names the biggest cuts), then a small job
re-running `voice-call-e2e-1` for a baseline number.

## Recommendation

Do 1 first — it is the cheapest and it makes 2–5 *trustable*. Then 2: the
path is already unblocked by one job. 3 and 4 can run alongside 2 — house
brain is HA config and the MoE tail is bench.run, neither touches the reel
branch. 5 comes after the latency map says where the money is.

Considered, not picked (already queued as research, or ops chores rather
than usefulness): `backup-audit`, `dashboard-ops-ideas`, `digest-design`,
`moe-scout` (feeds idea 4).
