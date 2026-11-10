# Claude Code `--resume` vs fresh round + continuation

**EVB-18** — the event-driven brain plan's resume experiment
(plan `PLAN-event-driven-brain.md` §3.5, plan task 18). Measured 2026-11-09,
run tag `11092112`, on the production stack: llama.cpp Qwen3.8-27B
Q4_K_M (`jarvis-engine`, :11434) driving Claude Code, real `cap-echo`
worker, real switchboard wake. Harness: `~/evb18-exp/` (scripts kept),
per-trial transcripts + `trial.json` under `~/evb18-exp/results/evb18-t<T><M>/`.

## Question

A job that **yielded** (`waiting` on a `cap.<name>` result) gets woken by
the switchboard when its result lands. How should the resuming round run?

- **Mode A — resume:** re-attach the exact Claude Code session that
  yielded: `claude --resume <session-id> -p <the same round prompt the
  runner would build>`. The session's own history (brief, snapshot, job,
  round-1 work) is already there.
- **Mode B — fresh + continuation:** a fresh Claude Code round through
  the production runner (`jarvis-jobs run`), with `continuation.md` and
  the result refs injected into the prompt (`=== CONTINUATION ===`
  block). This is the current production path.

Both modes see the same job, the same artifacts, and the same
instructions; only the launching differs.

## Tasks

Three small real tasks. In every one, **round 1 does real work and then
yields** on a genuine `cap-echo` result (the echo worker sleeps 30 s,
writes an artifact, replies on `flow.<job-id>`; the switchboard records
the result and wakes the job). The **resume round** must verify the
artifact and produce an output the harness checks independently:

| Task | Round 1 (before yield) | Resume round (after wake) |
|---|---|---|
| **T1** inbox + sha256 | write a 3-line inbox file | read the echo artifact, verify its `correlation:` line, write `out.txt`: the line containing "battery", verbatim + the artifact's sha256 |
| **T2** token verify | generate a random 8-byte token | read the artifact, verify `correlation:` and `job:` lines, write `out.txt`: `<token>:verified` |
| **T3** two echoes | submit **two** echoes, wait on both corrs | read both artifacts, verify both `correlation:` lines, write `out.txt` (`a:<corrA>` / `b:<corrB>`) and `out.json` (both `generated:` timestamps) |

Each mode got a **fresh job** (ids `evb18-t<T><M>`, `M` ∈ {a, b}); trials
ran sequentially, interleaved per task (t1a t1b t2a t2b t3a t3b) so both
modes of a task see the same worker load. Round 1 ran through the
production runner in both modes, so only the resume round differs.

## Measurements

Per round, from the stream-json `result` event: input tokens
(**fresh**, uncached), **cache-read tokens** (the llama.cpp engine
reports prompt-cache hits here — the number this experiment turns on),
output tokens, wall time, and an independent correctness pass (the
harness computes the expected `out.txt`/`out.json` itself and diffs).

## Deviations from production (honest list)

- **Mode A's prompt** is built by an exact replica of
  `Runner.Prompt()` (brief + machine snapshot + job header +
  continuation block, byte-for-byte, including the em dashes and ref
  priority). One deliberate difference: `inbox.Drain()` is not called
  (it consumes owner messages as a side effect); every trial's inbox
  was empty, so nothing was lost. Because the wired production path
  sends the *same* runner-built prompt (only the `--resume` flag is
  added — see Decision), mode A's measured token counts are
  representative of production, not a bound.
- Mode A is launched by hand (`agent-local --resume … -p …`), so the
  harness does `mark-running` before and `finish` after; the CLI's
  finish truncates the transcript, which the harness restores from its
  own capture. Mode B's round is the unmodified production runner.
- The round's child sessions write their own one-line section to
  `notes/conversations/agent-<date>.md` (the stop hook) — production
  behaviour, present in both modes.
- Every trial's fixtures (job, flow queue, listeners, artifact dir) are
  removed afterwards; only the transcripts and metrics under
  `~/evb18-exp/results/` remain.

## Results

All six trials: **correct — every harness check passed (18/18)**, and all
three resume trials re-attached the exact session that yielded
(`session_id` identical in round 1's init and the resume's result).

| task | mode | r1 in | r1 cache-read | r1 out | r1 wall | wake wait | res in | res cache-read | res out | res wall | correct | note |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| T1 inbox+sha256 | resume | 14690 | 145857 | 2375 | 97 | 0 | **4416** | 87479 | 2056 | 76 | yes | sid-match |
| T1 inbox+sha256 | fresh | 15469 | 143461 | 1783 | 83 | 0 | **16271** | 121172 | 6747 | 227 | yes | |
| T2 token verify | resume | 19159 | 142857 | 4628 | 145 | 0 | **4249** | 96377 | 780 | 27 | yes | sid-match |
| T2 token verify | fresh | 14228 | 143669 | 2247 | 77 | 0 | **14723** | 125811 | 2431 | 79 | yes | |
| T3 two echoes | resume | 17764 | 127758 | 3225 | 104 | 9 | **4510** | 96011 | 1300 | 40 | yes | sid-match |
| T3 two echoes | fresh | 14961 | 143662 | 1587 | 59 | 27 | **18983** | 129872 | 3408 | 111 | yes | |

Tokens per resume round: fresh input **4,249–4,510** (resume) vs
**14,723–18,983** (fresh) — resume saves **10.5K–14.5K fresh input
tokens per round (71–76%)**; 36,802 of 49,977 (73.6%) over the three
pairs. Total prompt tokens (fresh + cache-read): 92–101K (resume) vs
137–149K (fresh) — **28–33% less** prompt traffic per round. Output:
4,136 vs 12,586 total — **67% less** (output is the decode-bound,
most expensive part of a round).

Wake wait: in T1/T2 the echo result landed while round 1 was still
finishing (the child keeps working after its yield), so the job was
already `ready` at round-1 end (wait 0 s at the harness's 1-s
granularity); T3's two echoes are sequential on the single worker, so
round 1 ended genuinely `waiting` and the real wake came 9 s (resume)
and 27 s (fresh) later.

Transcript event span (first→last event; the closest thing to pure model
time, free of harness overhead and queueing):

| task | resume span | fresh span |
|---|---|---|
| T1 | 55.0 s | 55.9 s |
| T2 | 11.4 s | 42.9 s (3.8×) |
| T3 | 23.4 s | 90.8 s (3.9×) |

Per-check detail (all ok):

- T1a/T1b: `artifact` (correlation line matches), `out_line1`
  (the battery line, verbatim), `out_line2` (`sha256:` of the artifact,
  harness-recomputed)
- T2a/T2b: `artifact` (correlation + job lines match), `out_line`
  (`<token>:verified`)
- T3a/T3b: `artifacts` (both present, correlation lines match),
  `out_line1`/`out_line2` (`a:<corrA>` / `b:<corrB>`), `out.json`
  (both `generated:` values)

## Analysis

**Why resume is cheaper.** A fresh round's first request pays the whole
conversation bootstrap as fresh input — Claude Code's system prompt +
tool definitions + the round prompt, ~15–19K tokens with nothing to hit
in a brand-new session — and each later turn re-reads the growing
conversation from the engine's cache (the 121–130K cache-read column).
A resume round re-sends the yielding round's conversation (~90–96K
tokens), which the engine has *just processed* and serves from its
prompt cache, and pays fresh input only for the new round prompt
(4.2–4.5K — the replica prompt is the same text the runner would send,
so this includes the duplication of brief/snapshot the session already
holds, and the win still stands). It also produces less output: the
re-attached session already knows what it was doing; the fresh session
re-reads the continuation, re-verifies, and writes more.

**Wall time is directional only.** The single 4090 engine was shared
with the parent brain session for the first five minutes of the run
(parent active until 21:17:45Z); t1b's 227 s wall includes ~170 s of
pre-first-event queueing/startup that its model work (55.9 s event
span) did not include, and the resume rounds' walls include ~15–21 s of
harness overhead (mark-running, prompt build incl.
`jarvis-stack-status`, startup). Resume's wall is shorter in all three
pairs (76/27/40 vs 227/79/111), but the clean signals are tokens (above)
and the event span (T1 tie, T2/T3 ~4×).

**`duration_api_ms` is unreliable on this engine** — it exceeds the
process wall in 4 of 6 resume rounds (e.g. 168 s of "API time" in a
27 s process) and the event span in 3 of 6 fresh rounds. It was excluded
from the analysis entirely.

**Cache-warmth is the precondition, and it holds in production.** The
history hit depends on the engine still holding the yielding round's KV.
The engine's slot-save/restore wiring exists in the runner but is a
no-op in production (`Runner.EngineURL` unset), so the warm cache is the
llama.cpp server's own in-memory slot retention — and the experiment
shows it survives the production sharing pattern: t1a's resume round ran
*interleaved with parent-session requests* (parent turn 21:14:08–21:14:59
inside the resume's 21:14:18–21:15:34 window) and still banked 87,479
cache-read tokens. The one real cold case is an engine restart between
yield and wake (the photo swap window stops the brain container): the
resume then pays a one-time full-history prefill (~90–100K fresh tokens,
tens of seconds) — more than a fresh round's first request, but bounded
to one prefill, after which the round runs warm and its total prompt
traffic still lands below a fresh round's.

**Robustness.** Resume degrades to the status quo, never to a failure:
no continuation, no recorded session id, or no session file on disk all
fall back to the fresh round, which is exactly what every woken round
did before this experiment. The session id is recorded by the runner at
yield time (plan tasks 16/18), so jobs that yielded before the feature
simply resume the old way once.

## Decision

**The experiment wins; resume is the default for woken rounds.**
The plan's rule was "keep the fresh round as default unless the
experiment wins" — it won on every cost metric (−71–76% fresh input,
−28–33% total prompt, −67% output) at equal correctness (18/18 checks),
and won the model-time span on T2/T3 and tied T1.

Wired (this change):

- `Runner.Run` (`internal/agentjob/run.go`): when the job's
  continuation records a `SessionID` **and** the session file
  (`~/.claude/projects/<slug>/<sid>.jsonl`) exists, the round command
  gets `--resume <sid>` inserted after argv[0]. Everything else —
  prompt, env, drivers — is unchanged; `superagent` rounds (not Claude
  Code) are excluded. Any miss falls back to the fresh round.
- `Runner.finish`: a round that yielded records its session id from the
  transcript into `continuation.json` (`streamSessionID` +
  `recordSessionID`, job-locked RMW) — the "Empty until agent-run
  records it" half of the `Continuation.SessionID` field comment
  (plan tasks 16/18).
- `scripts/agent-run`: a comment at the round-launch site pointing here;
  no shell logic (the decision is in the runner).

Cost: one file dependency (the session's `.jsonl`, a few MB) and the
cold-restart prefill case above. Both accepted.
