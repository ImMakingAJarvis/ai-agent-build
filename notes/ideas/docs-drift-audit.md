# docs-drift-audit — system-frame.md is the stale core; four docs need a sync, and a test can keep them honest

Audit of `docs/` + `specs/` against the live stack (snapshot 2026-11-19 23:22,
`/etc/systemd/system`, `docker-compose.yml`, `/usr/local/bin`, `/opt/jarvis-*`).
**Ports are clean** (11434/11435/8100/8443/8080/8101/8096/1883/9104/9101… all match
live), and the new docs (brain-swap, event-driven, agent-run "Photo window",
igpu-model-handler, ha/README) are current. The drift is concentrated in the
inventory tables, which froze at different eras:

## Findings (file:line → fix)

**docs/system-frame.md — worst offender (predates the event-driven era):**
1. §5.1 container table (L243-262) missing 11 running services: `switchboard`,
   `cap-echo`, `sched-tick`, `sched-audit`, `token-ledger`, `image-gen-worker`,
   `image-describe-worker-{gpu,cpu}`, `audio-transcribe-worker`,
   `llm-small-worker-{gpu,cpu}`, `bench-worker` (+ optional `engine-voice`;
   `review-{sonnet,opus,fable,sweep}` pending on phase-pr-review-queues).
   → add rows; it is the "whole Jarvis in one document" and is 1/3 of the stack blind.
2. §5.2 host units (L266-278) missing `task-runner`, `resourced` (the running
   window controller — only in services.md), `jarvis-think-{digest,topup}.timer`
   (enabled); **duplicate rows** (jarvis-engine-igpu.service twice L269/270,
   legacy catch-all twice L277/278 — merge artifact). → add + de-duplicate.
3. §5.3 CLIs (L282-288) lists `enqueue` and `jarvis-migrate` — **not installed**
   (checked /usr/local/bin) — and omits 13 that are: `jarvis-mq`, `jarvis-swap`,
   `jarvis-review`, `jarvis-kanban`, `jarvis-secrets`, `jarvis-stack-status`,
   `jarvis-photo`, `jarvis-art`, `agent-run-restart`, `gh-pr-wait`, `superagent`,
   `jarvis-watchdog`, `plane-migrate-projects`. → rebuild from the live dir.
4. §4.2 "The two job queues" + §9 deep-docs index (L362-377): the event-driven
   layer (flow/cap/mailbox/events queues, switchboard) is absent; §9 does not link
   `docs/event-driven.md`, `docs/brain-swap.md` or `docs/idle-work.md`. → add a
   third "queue" paragraph + index links.
5. §6 kanban row (L305): "(no comment system yet)" — comments are live
   (`jarvis-kanban comment`, `/api/kanban/comments`). → delete the parenthetical.
6. §5.1 prometheus row (L258): "15 d retention" — compose pins
   `--storage.tsdb.retention.time=180d` (services.md correctly says 180 d). → 180 d.
7. §8 "Known deviations (2026-11-05)": stale date and missing the live deviation —
   `evb37-demo` (reel:local, Up 4 days) runs **outside compose**. → refresh §8.

**docs/services.md:**
8. "17 services (18 with --profile optional)" (L82) — compose now has ~30 default
   services on main (+voice +optional profiles, +4 review-* on the open branch).
9. Verify block (L85-86) curls `127.0.0.1:8120/health` as a standard check, but
   `jarvis-art`/`jarvis-bench` are `exited` in steady state (window-scoped; the
   photo executor starts art on demand). → mark art/bench "stopped between photo
   windows" and gate those curls.
10. "pinned llama.cpp **b11815**" (L73, L137; also specs/engine.md L18 and the
    compose comment L51): the running binary self-reports
    `version 0.7.0-dev (build 1, commit 7170cad4d)` — a source build, not the
    b11815 release. → pin the docs to the real commit and write a `BUILD.txt`
    marker into `/opt/jarvis-engine` on install so the next swap is detectable.

**docs/dashboard.md:**
11. "Inhibit" section (L109-115): `systemctl stop/disable jarvis-dashboard` and
    the `touch /var/lib/jarvis/dashboard.disabled` switch are dead — the unit is
    disabled and the container never reads the file (only the legacy
    `scripts/install_dashboard.sh` mentions it; the file on disk is a relic).
    "D0.3 should reuse this unit" is also stale future-tense. → rewrite for the
    container era (`docker compose stop dashboard`); the file switch stays
    documented only for the rollback unit.

**docs/work-board/NOTES.md + repo CLAUDE.md:**
12. NOTES.md is self-contradictory: early sections (L54-94, L158 "watching AAB")
    describe the single-AAB world, superseded by the 2026-10-31 reorg section
    (one project per epic, AAB legacy). The identifier list (L349) has 8 of the
    15 live projects — missing `GLM, RCL, EVB, DRY, WRD, CTX, REV` (live
    `PLANE_PROJECTS`). CLAUDE.md's Plane section repeats the 8-id list and says
    "task-runner (kanban; dashboard on :8080)" — :8080 is the `jarvis-dashboard`
    container, not task-runner. → collapse NOTES.md to the post-reorg standard,
    sync the identifier list from agent.env, fix CLAUDE.md.

**Cleanup (optional, same PR):** 8 stale `*.bak*` binaries in /usr/local/bin
(jarvis-jobs/mq/swap, resourced, plane-bridge, task-runner, jarvis-watchdog);
dead files `/var/lib/jarvis/dashboard.disabled`, stray
`/var/lib/jarvis/qwen35-claude-code.jinja`.

## What is NOT stale (verified, leave alone)

- All port claims; all unit-state claims (legacy units correctly "disabled,
  rollback only"); wake-queue (spec, compose profile and §8 agree: optional, not
  running); brain-swap.md, agent-run.md photo-window, event-driven.md (EVB-38
  as-built), igpu-model-handler.md, ha/README.md; specs/*.md systemd deploy lines
  (plane-bridge/task-runner/jarvis-watchdog are genuinely still systemd units).
- `specs/specs_test.go` already enforces spec↔compose/unit/cmd coverage both
  ways — that is why the spec layer shows zero drift.

## Options

1. **One docs-sync PR** fixing items 1-12 across the four files. Small, pure
   docs, low risk. (Recommended.)
2. **Add a drift guard** in the same or a follow-up PR: extend
   `specs_test.go` (or a `scripts/docs_drift_check.sh` run by it) to fail when a
   compose service, enabled systemd unit, or /usr/local/bin CLI is missing from
   the system-frame.md §5.1/§5.2/§5.3 tables — the same coverage pattern the
   specs already use. ~50 lines; turns this audit into a regression test.
3. **Generate the inventory tables** from compose + systemctl into the markdown
   (single source of truth). Heavier: tables lose hand-written "what it does"
   prose unless generated rows are merged in; overkill for a 30-service stack.
4. **Do nothing** until the next big topology change — the drift will just
   re-accumulate; this audit took an evening of grepping to rebuild.

## Recommendation

Option 1 + option 2 as one PR: fix the four files now (system-frame.md is the
priority — it is the onboarding map and is missing a third of the stack), then
make the check permanent so "inventory table vs live stack" stops being a
manual archaeology exercise. Keep option 3 parked; revisit only if the stack
doubles again.
