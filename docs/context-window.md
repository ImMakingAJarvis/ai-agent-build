# Context window guard

A local-brain round runs inside a **fixed** engine window: 128K tokens
(`--ctx-size 131072`) on the brain, 32K on the iGPU helper. The harness
estimates tokens with its own tokenizer; the engine counts with the
model's. When the estimate runs hot, one long turn can push the *real*
prompt past `--ctx-size` and the API call dies mid-round — the round is
lost with its work half done. The harness auto-compacts near the top of
the window, but that leaves no room for the mismatch.

The guard closes that gap: a PreToolUse hook
(`scripts/context-guard-hook.sh`, registered in `.claude/settings.json`)
runs **before every tool call** in a job round, measures the actual
prompt size from the last API usage in the session transcript, and acts
on it in tiers.

## How it measures

The transcript (`~/.claude/projects/<project>/<session>.jsonl`) records
`message.usage` on every assistant line. The prompt that will be sent on
the *next* turn is the last usage's `input_tokens` +
`cache_read_input_tokens` + `cache_creation_input_tokens` plus the
`output_tokens` it just produced (all of it re-sends next turn). The
hook takes the newest usage within a bounded tail (8 MB) of the file —
it is always within one tool-result line of the end.

## Tiers

Thresholds are percentages of `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
(the window the harness was told about), overridable per round with
`CTX_GUARD_WARN` / `CTX_GUARD_TRIM` / `CTX_GUARD_HARD`:

| tier  | default | action |
|-------|---------|--------|
| ok    | < 65%   | state + log only |
| warn  | ≥ 65%   | one non-blocking reminder per 5% band: keep reads small, pipe long output through head/tail, don't re-read files |
| trim  | ≥ 75%   | one reminder per band: write progress to `notes/` now (a fresh conversation can resume), stop growing the context, yield if the job can wait |
| hard  | ≥ 85%   | **blocks** context-heavy tools (Read, WebFetch, WebSearch, Agent, NotebookEdit, and non-whitelisted Bash) with the compress-or-exit instruction below |

Reminders ride on the hook's `additionalContext` output (the same
mechanism as the owner-message hook) — exit 0, injected into the model's
next turn. The 5% band throttle (`pct / 5`) means a hot round gets at
most a handful of nudges, not one per tool call; a new session resets
the bands.

At the hard tier the blocked message gives the model three options:

1. **Yield** the job (`jarvis-jobs yield $JARVIS_JOB_ID --note ... --in-flight ... --next ...`)
   when it can actually wait — a corr, a review, a window.
2. **End the round now**: write the state to `notes/` (files changed,
   decisions, exact next steps) and stop calling tools. The loop
   checkpoints; the next round resumes from the notes.
3. Take only tiny steps until the harness auto-compacts.

The escape hatches are never blocked: `jarvis-*` helpers (yield, finish,
discord, notes, secrets…), `git`/`gh`, and the small utilities
(`echo ls cat head tail wc mv cp mkdir rm touch date whoami true`).
Write and Edit are never blocked — the model must always be able to
compress (write notes) or exit. And a round can always simply stop
calling tools; PreToolUse never fires on the final text.

## State and observability

Every check (that measured a usage) writes, atomically:

- `~/.local/state/jarvis/context-window.json` — job id, session id,
  model, window, prompt tokens, pct, tier, timestamp, throttle state.
  Read by `jarvis-stack-status` (the "Context window (last check)" line,
  `(stale)` when older than 15 min, and `report["context"]` in `--json`).
- `~/.local/state/jarvis/context-window.log` — one line per check,
  appended on a tier/band change or at most every 15 s; rotates to
  `.log.1` past 500 KB.

`CTX_GUARD_STATE_DIR` moves both (tests use this).

## The output-token cap

A single response must never be able to push a hot window past
`--ctx-size` on its own. `CLAUDE_CODE_MAX_OUTPUT_TOKENS` is therefore
capped at the model's hard output limit (Qwen3.8: 32768) — in
`scripts/agent-local` (default when unset) and in the `agent-run`
systemd drop-in `systemd/agent-run.service.d/output-tokens.conf`.
It used to be 262144 (256K — twice the window), which was the actual
overflow vector. The iGPU driver caps output itself (16384) in
`internal/agentjob/run.go`.

## Fail-open

The hook exits 0 on anything unexpected (no `JARVIS_JOB_ID`, no numeric
window — sonnet/opus/interactive sessions are not guarded — no `jq`,
no transcript, no usage yet in the tail, unreadable state). It can
never be the reason a round cannot run.

## Tuning

- Per round: `CTX_GUARD_WARN=60 CTX_GUARD_TRIM=70 CTX_GUARD_HARD=80
  jarvis-jobs run …` (env of the round).
- Permanent: edit the defaults at the `warn=… trim=… hard=…` lines in
  `scripts/context-guard-hook.sh`.
- The hard-tier whitelist is the `case "$word"` block; add a command
  name there when a round gets stuck on something it legitimately needs
  to call while hot.

## Tests

`scripts/context_guard_test.sh` — table-driven, follows
`test_brain_guard_hook.sh`. It builds fake transcript files with
assistant usage lines at controlled prompt sizes and checks: low →
exit 0 + state written; warn band → exit 0 with a reminder in stdout,
and a *second call in the same band* gets no reminder (throttle);
hard → Read blocked (exit 2), `jarvis-jobs yield …` allowed, a
non-whitelisted Bash command blocked; no `JARVIS_JOB_ID` → exit 0;
transcript with no usage → exit 0. Run it with
`bash scripts/context_guard_test.sh`.

## Deploy

No service restart: `scripts/context-guard-hook.sh` and
`scripts/agent-local` are re-exec'd every round, so the `/usr/local/bin`
copies (agent-local) plus the repo's `.claude/settings.json` (read at
session start, so the hook is live from the next round) are enough. The
output-token drop-in needs a `systemctl daemon-reload` and takes effect
on the next `agent-run` start — never restart a live round for it.
Deploy: copy `scripts/agent-local` to `/usr/local/bin/agent-local`,
`systemctl daemon-reload`, done.
