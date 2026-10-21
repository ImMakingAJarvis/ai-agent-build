# J0 — Make the queue the source of all work

Read only the section for your job. Background, shared by all J0 jobs:

- Intent: all work, build work included, lives on the queue, and TASKS.md becomes history. Today `agent-run` starts every round from the stage-3 brief (`STAGE3L` and `STAGE3S` in `bootstrap.sh`, written to `~/.config/jarvis/stage3-*.txt`), which says to work through TASKS.md.
- `cmd/worker/main.go` runs a local job as one tool-less `ollama.Generate` call and discards the output. The API path logs "not yet implemented" and still marks the job completed.
- RabbitMQ closes a channel whose message stays unacked past the consumer ack timeout (30 min by default), and a build round takes longer than that. So jobs are claimed in a store and acked at once, never held unacked.
- J1 later moves the job store into PostgreSQL. Keep the store behind a small Go interface so J1 only swaps the implementation.

A Sonnet round started at 16:00 on Oct 21 on all of J0. Check `gh pr list --state all` and the branches first, and continue from the first job not done.

## J0.1 — Worker stops faking results

- Files: `cmd/worker/main.go` and the history writer it uses.
- Store the text a local job generates in its history record (field `output`, trimmed to 20 KB). Mark API-routed jobs `failed` with "API dispatch not implemented". If any path still acks a job while its queue is paused, change it to leave the job unacked.
- Done when: unit tests cover a text job's output being stored and an API-routed job being `failed`, not `completed`. `go build ./... && go test ./...` passes.

## J0.2 — Agent job message fields

- Files: `internal/queue/message.go`, `cmd/enqueue/main.go`.
- Add optional fields: `kind` (`"agent"` or empty for text jobs), `title`, `instructions`, `doc` (path to its job doc section), `branch`, `depends_on` ([]string), `driver` (`local` or `sonnet`). Add matching `-kind -title -doc -branch -depends-on -driver` flags to enqueue.
- Done when: JSON round-trip tests pass and old messages without the fields still decode.

## J0.3 — Job store with claims (file-backed for now)

- New package `internal/jobstore`: interface `Store` with `Add`, `NextReady` (highest priority, then oldest, all `depends_on` done), `Claim(id, round)`, `Start`, `Finish(id, status, prURL, summary, err)`, `List`. Implement it on a JSON file under `~/.local/share/jarvis/jobstore.json` with a file lock.
- A claimed job with no result goes back to ready on the next load, unless its claim is less than 12 hours old and its process is still alive.
- Done when: tests cover ordering, dependencies, double-claim prevention, and recovery of a stale claim.

## J0.4 — `jarvis-jobs` CLI

- New `cmd/jarvis-jobs`: `next` (prints the next ready job as JSON), `claim <id>`, `done <id> [--pr URL] [--summary S]`, `fail <id> --error E`, `list`. It uses `internal/jobstore`. A small consumer moves agent jobs from RabbitMQ into the store and acks them at once.
- Done when: CLI tests run against a temporary store, and an agent job enqueued with `jarvis-enqueue -kind agent` shows up in `jarvis-jobs list` within 5s.

## J0.5 — `agent-run` takes the next job from the queue

- File: `scripts/agent-run`, the stage-3 loop only.
- Before each checkpoint, call `jarvis-jobs next`. The Discord question names it ("Next: D0.1 — … Local model / Sonnet / Skip / Stop"). On Local model or Sonnet: claim it, run the driver with the stage-3 rules plus the job's title, its doc section and its instructions (not TASKS.md), then mark it done with the PR URL from `gh pr list --head <branch>`, or failed. Skip leaves it ready and offers the next one. Typed text still goes to the local model as extra instructions.
- Install the new script atomically (copy to a temp name in `/usr/local/bin`, then `mv`), because a running `agent-run` reads its script as it goes. Never kill a running round.
- Done when: a dry run with a fake driver (`AGENT_RUN_FAKE=1`) claims, runs and finishes a test job. The real checkpoint shows the next job's title.

## J0.6 — Briefs say "do the job you were handed"

- Files: `bootstrap.sh` (`STAGE3L`, `STAGE3S` heredocs only; grep for them, don't read the whole file), plus the live `~/.config/jarvis/stage3-local.txt` and `stage3-sonnet.txt`, replaced atomically.
- Replace the "take the next unfinished item … in TASKS.md" part with: do the one job you were handed, follow the job rules at the top of TASKS.md (stay within context, split big jobs, read in chunks), and end with a PR.
- Done when: `bash -n bootstrap.sh` passes, a diff of the live files shows only that change, and the next round's prompt (visible in `ps`) contains the new text.

## J0.7 — Move TASKS.md work onto the queue

- New `scripts/migrate_tasks_to_queue`. It reads the "Queued jobs" index in TASKS.md (lines like `- [ ] **D0.1 — title** · docs/jobs/d0-dashboard.md`) and enqueues each unticked line, in order, as an agent job with `doc` set and `depends_on` set to the previous line in the same group. Then it adds every other unticked item in TASKS.md as a job, in file order, with the item text as the instructions. Each job's ID is its index ID, or a slug for the older items.
- It is idempotent: a second run adds nothing, and it updates the instructions of jobs that are still ready when their text changed. Then add a note at the top of TASKS.md: "Work now lives in the queue (see the dashboard); this file is history." Tick J0.
- Done when: a test run against a copy of TASKS.md gives the right order and dependencies, and a second run adds nothing.

## J0.8 — Dashboard shows agent jobs

- File: the dashboard template and handler only.
- Show agent jobs with ID, title, status (ready, claimed, running, done, failed), driver, PR link and dependencies.
- Done when: a template test with fixture jobs renders every status, and the page shows the migrated jobs with J1.1 first.

## J0.9 — Migration reads the index format (do this before running the migration)

- Branch: `phase-3-queue-agent-jobs` (the J0 PR). First rebase it onto `main`. On the `TASKS.md` conflict, keep main's version: main replaced the long Oct 21 section with this index and ticked J0.1-J0.8.
- File: `internal/agentjob/migrate.go` and its test only.
- Change `codeRe` so dotted IDs stay whole: `D0.1` must give ID `d0.1`, not `d0`. Today `^\*{0,2}([A-Z]\d+)\b` stops at the dot, so every job in a group collides.
- For index lines that name a job doc (`` · `docs/jobs/<file>.md` ``), set the job's instructions to that job's own `## <ID> — …` section from the doc, plus the doc's background paragraph at the top, instead of the whole index line. Store the doc path in the job.
- Ticked lines (`- [x]`) are skipped, as today.
- Done when: a test against main's `TASKS.md` and `docs/jobs/` gives unique IDs `j0.9`, `j1.1` … `q1.3` in index order, each with only its own doc section as instructions; a second run adds nothing; `go test ./...` passes.

## J0.10 — Job rules in the briefs and the agent-job prompt

- Rounds no longer read TASKS.md, so the "Job rules" block at the top of TASKS.md has to reach the model another way. Copy it, nearly word for word, into `STAGE3L` and `STAGE3S` in `bootstrap.sh` (grep for the heredocs), the live `~/.config/jarvis/stage3-local.txt` and `stage3-sonnet.txt` (replaced atomically), and the prompt that `internal/agentjob/run.go` builds for each job.
- Also add: "If the job is too big, split it: add the smaller jobs to the queue with `jarvis-jobs add` (or `jarvis-enqueue -kind agent`) right after this one, with their own acceptance criteria, then do only the first. Mark this job done with a note that it was split."
- Done when: `bash -n bootstrap.sh` passes, a test checks the job prompt contains the rules, and the next round's prompt (visible in `ps`) contains them.
