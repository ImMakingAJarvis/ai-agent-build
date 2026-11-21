# digest-design — one DM: decisions first, ideas second, health and cost in one line

2026-11-20, think job. Sources: `scripts/think_digest.py` (127 lines, on main
since 17f4ef6), its systemd timer + 11-19 journal, `notes/ideas/*` (10 notes),
`notes/agent/asked.md`, the job store (`state.json` fields),
`docs/idle-work.md`, `specs/jarvis-discord.md`.

## What I found

- **The digest has never delivered.** The 11-19 08:00 run failed with ENOENT —
  the timer runs `scripts/think_digest.py` from the working tree, and the tree
  was on a branch that didn't contain the file. The script is on main, so any
  branch based on current main works; a stale-based branch breaks the 08:00 run.
  Today's 08:00 run (Persistent=) will be the first real digest — and it faces
  up to 10 fresh notes, which the current 8-title cap would send as a bare list.
- **The current digest is titles + counts + a question.** Correct instinct
  (silent when idle, marker-gated, one message), but it omits the four things
  an owner actually checks on waking: (a) what needs *him* — PRs opened or
  touched overnight, `NOT VERIFIED LIVE` items, open questions; (b) which jobs
  **failed and why** — it reports a count, and the queue already carries 62
  failed; (c) one line of stack health; (d) what the night **cost** — every
  job records `kwh` (`JARVIS_POWER_RATE` is 0 by default, so cost is kWh-only
  until Nathan sets a rate).
- **The notes already summarize themselves.** Every note's title is the
  takeaway ("62 failed jobs: 57 stale, 5 worth requeueing"). The digest should
  not re-summarize notes — it should list title + path and spend its budget on
  the ops bands the notes can't carry.
- **All needed data is local and model-free at 08:00:** job store
  (`status`, `finished_at`, `error`, `kwh`), `gh pr list` via
  `GH_TOKEN=$(ghapp-token)`, `jarvis-stack-status --json`, `notes/ideas/*.md`.
  Nothing requires a brain round.
- **Constraints:** one plain Discord message (2000-char hard limit — budget
  ≤1200), one DM (not two), silent-when-empty stays (an idle night is not an
  event), the marker is already written only after a successful notify.

## Options

1. **Minimal patch** — current digest + kWh line + cap notes at 6. ~20 lines.
   Fixes the 10-note overflow, adds cost, still says nothing about failures or
   PRs.
2. **Night report (recommended)** — keep the notes list, add three bands:
   *needs you* (PRs + unverified items), *failures* (id + one-line error), and a
   closing health+power line. All from sources above; degrades to option 1 if
   any source fails.
3. **DM + dashboard page** — short DM links a `:8080/digest` page with full
   detail. Nice, but a new page for something read once a day; the notes
   already live in `notes/ideas/`.
4. **Model-written digest** — a 27B round at 08:00 reads the notes and writes
   the summary. Better prose, but costs a round, is nondeterministic, and
   fights the swap window (the brain may be out exactly then). Rejected as
   default; the notes' titles already do the summarizing.
5. **Split DMs** — ideas in one message, ops in another. Two 08:00 pings is
   worse than one ordered message. Rejected.

## Recommendation — spec for `scripts/think_digest.py`

**Shape** (order = attention order; `…` = only when non-empty):

```
Night report — 4 jobs done, 1 failed, 3 idea notes, 0.11 kWh
Needs you:
- PR #274 review-queues open: <url>
- PR #271 NOT VERIFIED LIVE: voice swap window
Failures:
- plan-…-4: ECONNREFUSED :11434 at round start
Ideas:
- failed-triage — 62 failed jobs: 57 stale, 5 worth requeueing (notes/ideas/failed-triage.md)
- …(up to 6, then "…and N more in notes/ideas/")
Health: OK — 29 up, both engines, queue ready=82        |  Health: jarvis-voice DOWN — :8080
Which of these should become real work?
```

- **Window** — keep the epoch marker; select jobs by `finished_at` inside the
  window, not `state.json` mtime (mtime lies when the store is rewritten).
- **Needs you** — open PRs with `updatedAt` in the window (gh, token via
  `ghapp-token`), plus any open PR whose body starts with `NOT VERIFIED LIVE`.
- **Failures** — jobs with `status=failed` and `finished_at` in the window;
  list at most 3 as `- <id>: <first line of error>`, then "…and N more
  (Agent Jobs page)".
- **Ideas** — as today, cap 6.
- **Health** — one line from `jarvis-stack-status --json`: OK summary, or the
  names of what is down + a pointer to :8080.
- **Power** — sum `kwh` over window jobs in the headline; append `$X.XX` only
  when `JARVIS_POWER_RATE > 0`.
- **Budget & trim** — hard cap 1900 chars; trim overflow ideas first, then
  failure lines; never trim Needs-you or the closing question.
- **Degrade** — each optional source (gh, stack-status) failing skips its band
  and the DM still goes out; only a `jarvis-discord notify` failure blocks the
  marker (already the case).
- **Dry run** — add `THINK_DIGEST_DRY_RUN=1` (print the message, no DM, no
  marker) so the format can be checked without waking Nathan.
- **Note contract** — keep the de-facto standard: the title *is* the takeaway,
  one line, ≤110 chars. State it in `docs/idle-work.md`'s output section and
  the backlog rules so future notes stay digest-able.
- **Cut** — note bodies, job logs, full stack status, photos (already DM'd on
  delivery), benchmark numbers (they have the /benchmarks page).
- **Ops caveat** — if the 08:00 run ever ENOENTs again (tree on a stale
  branch), re-run by hand (`python3 scripts/think_digest.py`, Persistent= has
  already retried); the durable fix is keeping the tree's branch based on
  current main, which the loop already does after each merge.
