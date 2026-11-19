# Idle work — the machine is never idle

Nathan's ask (voice, 2026-11-18, job `chat-1795024561-0aff`): the AI should
always be doing something at night. The box doubles as a heater for his
mother's place, so when there is no real work the GPU should still be
earning its power draw — ideally making money (a render network), at minimum
doing useful thinking. This is the system for that.

## The priority ladder

What the 4090 does, in order of preference:

1. **Real queued work** — fix/build/background agent jobs. Always wins.
2. **Render / GPU-share work** *(aspirational, not yet connected)* — a
   render network that pays for idle GPU hours. See
   [render-network.md](render-network.md). When it exists it slots in here:
   real work first, then paid idle, then thinking.
3. **The thought loop** — this document. Bounded thinking jobs (research
   questions, project ideas, audits) that are always present at the bottom
   of the queue, so the loop has something to run whenever the real queue
   drains — which is what nights are.

Thinking is the standing fallback: it costs only local inference (the kWh
per job is already tracked, `docs/agent-jobs.md` "Power cost"), it produces
notes Nathan can react to in the morning, and it keeps the machine warm.

## The thought loop (built)

- **Think jobs** are ordinary agent jobs with the `think` tag,
  `class=background`, `priority=-100` (below every real job — verified:
  all existing priorities are ≥ 0), `driver=local`, `effort=low`.
  `jarvis-jobs next` therefore returns one only when nothing else is ready.
- **Backlog** — `config/think-backlog.md`: one line per thinking task,
  `- <slug>: <description>`. The slug becomes the job id `think-<slug>`.
  The job store is the claim ledger: `Ingest` is idempotent, so a done
  think job can never be re-queued (its id still exists).
- **Top-up** — `scripts/think_topup.py`, run every 15 min by
  `jarvis-think-topup.timer`. While fewer than `THINK_FLOOR` (default 3)
  think jobs are active (ready/claimed/running/waiting), it adds the next
  unclaimed backlog entries. Missing backlog or store = logged no-op.
- **Self-replenishment** — when the backlog runs out, the top-up enqueues
  one `think-refill-<YYYYMMDD>` job per UTC day: the agent brainstorms and
  appends 5–10 new backlog entries (same bar: concrete, bounded, one note
  each), so the loop never dies.
- **Output** — each think round writes `notes/ideas/<slug>.md` (1–2 pages:
  findings, options, a recommendation) and one `jarvis-notes add done`
  line, so the note is in the vector graph the same session. Think jobs
  ship no code and open no PRs — `notes/**` stays working-tree-only, and
  follow-up jobs are Nathan's call, not the agent's.
- **Morning digest** — `scripts/think_digest.py` at 08:00 local
  (`jarvis-think-digest.timer`, Persistent) DMs the notes written since the
  last digest plus done/failed counts, ending with "which of these should
  become real work?" No notes, no message.
- **Install** — `sudo bash scripts/install_think.sh` (units in `systemd/`,
  same pattern as `install_agent_run.sh`).

## Turning it on

The loop only runs autonomously under NoBrakes (docs/nobrakes/):

```
!fullauto on     # the agent-run loop grabs the next ready job without asking
!automode on     # optional: also auto-enqueue parked nightly-tagged jobs
```

With `full_auto` off (the default), the loop asks Nathan on Discord who
runs the next job — the think jobs then just sit at the bottom of the queue
and cost nothing. Turning the switches on is a deliberate operator act; a
failed auto job pauses both switches and DMs the owner (docs/nobrakes/safety.md).
Think jobs get the same guardrails for free: at most one auto job at a time,
pause-on-failure (or notify-and-continue with `!keepgoing on`).

Kill switches: `!fullauto off`; `sudo systemctl stop jarvis-think-topup.timer`
(the queue keeps its think jobs — skip them with `jarvis-jobs skip <id>`);
`~/disable-agent-run` stops everything.

## Cost

A think round is a local 27B agent round: no API spend, only electricity
(the per-job `kwh`/`power_cost` already land in `state.json` and the
dashboard). Three think jobs in the floor means the queue never empties, so
a drained night runs think rounds back to back. First-order number for the
box's all-in bill per mode: see the `power-economics` backlog entry.

## What this is not

- **Not a render farm.** Paid idle compute needs a network choice and a
  wallet — that decision is docs/render-network.md and a question for
  Nathan. The ladder has a slot for it; when it lands, it outranks thinking.
- **Not unbounded autonomy.** Think jobs cannot queue follow-up work,
  cannot open PRs, and run at the lowest priority with the lowest effort.
  The output surface is a note plus a morning DM.
- **Not Plane.** Idea tracking lives in `notes/ideas/` + the digest for
  now. A `plane-epic create "Project ideas" --identifier IDEA` project is
  the obvious upgrade if the idea stream stays interesting (Nathan's call).

## Operations

```
systemctl list-timers 'jarvis-think*'          # next ticks
journalctl -u jarvis-think-topup -n 20         # top-up log
jarvis-jobs list ready | grep '^.*think'       # active think depth  (via store)
jarvis-jobs skip think-<slug>                  # drop one
THINK_FLOOR=1 ...                              # env overrides on the unit, if ever needed
python3 scripts/think_topup.py                 # run one pass by hand
python3 scripts/think_digest.py                # run the digest by hand (DMs!)
```
