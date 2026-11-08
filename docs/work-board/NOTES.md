# Work Board Notes

Append-only decisions from each task of `PLAN.md` (one section per task, in order).

## P1 (2026-10-31) — Plane deployment verified

- Plane CE is live on the Synology NAS `TempleOfTime` at **111.1.2.222**,
  reachable from the AI box at **http://111.1.2.222:8088/** (login page
  loads; no DNS name on the LAN, no `nathan.homes` records exist).
  Fronted by Caddy (`Via: 1.1 Caddy`), backend `Server: uvicorn`
  (2026 Plane build: Django+DRF on ASGI).
- This Plane's API split (from the makeplane/plane source, verified live):
  - **External API (API key): prefix `/api/v1/`**, header **`X-Api-Key: <key>`**
    (NOT `Authorization: Bearer` — that 401s with "credentials were not
    provided").
  - Session API (browser cookies, what the web app uses): prefix `/api/`.
  - Routes are workspace-scoped: `/api/v1/workspaces/<slug>/projects/<id>/...`,
    incl. `work-items/` (issues), `states/`, `labels/`, `members/`;
    `/api/v1/users/me/` is the unscoped identity endpoint.
  - OpenAPI schema endpoint (`/api/schema/`) is not enabled on this deploy.
- The key Nathan provided (`/home/agent/planekey.txt`, `plane_api_…`, 42
  chars) authenticates as **his admin account** (nathan219@gmail.com), not a
  bot. Verified: `GET /api/v1/users/me/` with `X-Api-Key` → 200 with the user
  JSON. The Jarvis bot member + its own key is created by P2 (`plane-setup`).
- NAS access facts: SMB anonymous/guest is disabled; no NAS creds on the AI
  box; NFS closed. The Plane key was dropped by Nathan at
  `/home/agent/planekey.txt`. Pending chat job chat-1793441273-bca6 wires
  `PLANE_URL`/`PLANE_API_KEY` into the env config the daemon reads.

## Env wiring (2026-10-31) — PLANE_URL/PLANE_API_KEY in the daemon env config

- Wired into `~/.config/jarvis/agent.env` — the `EnvironmentFile` read by
  agent-run, agent-resume, jarvis-chat and wake-queue (and optionally by
  the dashboard/HA units). Daemons pick the vars up at their next service
  start; nothing consumes them yet (plane-bridge arrives with P2+), so no
  restart was needed.
- `PLANE_URL=http://111.1.2.222:8088`. The requested
  `https://plane.nathan.homes` does not resolve from the AI box
  (re-checked 2026-10-31, `getent` + curl both fail); the IP URL is the
  one verified live in P1. Flip `PLANE_URL` to the hostname if the
  `nathan.homes` DNS record lands.
- `PLANE_API_KEY` = the 42-char `plane_api_…` key from
  `/home/agent/planekey.txt`. Caveat for consumers: that file is 5 blank
  lines + the key on the last line — take the last non-empty line, not the
  whole file. Key stays out of the repo.
- Re-verified after wiring: `GET $PLANE_URL/api/v1/users/me/` with
  `X-Api-Key` → 200 (Nathan's admin account, per P1).
- The handoff file `/home/agent/planekey.txt` was left in place (Nathan's
  file); it can be shredded once the P2 bot key replaces it in config.

## P2 (2026-10-31) — idempotent `scripts/plane-setup.sh`

- Script: `scripts/plane-setup.sh` (bash, `set -euo pipefail`). Converges
  workspace `jarvis`, project `ai_agent_build` (identifier `AAB`), the 8
  planned states (seq 1000–8000), the `epic` label, the bot member, and the
  env file. First run creates everything; a re-run is a true no-op (verified:
  no create/patch/delete, env file untouched, `verify` ✓, exit 0).
- **`PLANE_API_KEY` now holds the bot's key** (the script rotates a labeled
  token `jarvis-bot` on the bot account and installs it). Nathan's key is
  preserved as `PLANE_ADMIN_API_KEY` (with `PLANE_ADMIN_EMAIL`); bot creds
  live in `PLANE_BOT_EMAIL`/`PLANE_BOT_PASSWORD`. The `planekey.txt` handoff
  is superseded by the env file.
- Bot account is **`jarvis-bot2@nathan.homes`**: an aborted first run created
  `jarvis-bot@` whose generated password was lost (Plane has no password
  reset without SMTP, no user-delete API) → the zombie account stays a
  (credential-less) bot member of the workspace; the member-delete endpoint
  filters `member__is_bot=False`, so it cannot be removed via API. Harmless.
- **Admin membership is a pending invite, accepted by a human.** Headless
  member-add is impossible on this build: the invite-join endpoint requires
  the invitee's own authenticated session *and* the one-time invite token
  (GHSA-4vj8-p63v-8p24), and no API ever exposes the token
  (GHSA-86mg-259g-pwgg). The script creates/keeps the pending admin invite
  (400 `EMAIL_ALREADY_INVITED` = already pending, no-op); no SMTP on the
  instance, so nathan219@gmail.com accepts it in the Plane UI.
- Build quirks hit (2026-10-31 deploy, behaves like Plane 1.4.x):
  - Session cookie is **`session-id`** (hyphen), not `sessionid`; the script
    matches both. CSRF token must be re-fetched after sign-in (it moves into
    the session).
  - Session `GET /api/workspaces/` 400s ("required key does not exist");
    the working list is `GET /api/users/me/workspaces/` (plain array).
  - Project **names** forbid hyphens (`FORBIDDEN_IDENTIFIER_CHARS`
    includes `-`) → `ai_agent_build`; identifier `AAB` is unaffected.
  - State `sequence` is stored/returned as a **float** (1000.0) — compare
    against the normalized integer or every run re-PATCHes all states.
  - This build's auto-created default states are 5 (Backlog, Todo,
    In Progress, Cancelled, Done); the script removes any state not in the
    planned set unless a work item references it.
  - Workspace creation auto-creates a default project named after the
    workspace (`Jarvis`/`JARVI`); left in place (outside the job's object
    list — no destructive ops beyond it). Board work targets `AAB`.
  - State matching is case-insensitive, so the planned "In progress" keeps
    Plane's default capitalization "In Progress" (PATCHed, not duplicated).
- Acceptance evidence: independent re-list with the env-file bot key
  (`X-Api-Key`) → 1× `ai_agent_build`/AAB, exactly the 8 states in order
  (no duplicates), 1× `epic` label, members = bot2 (+ the credential-less
  zombie bot), 1 pending admin invite (accepted=False).

## P3 (2026-10-31) — Go plane client package

- Package: `internal/plane`. Covers the full P3 scope — create work item
  and sub-item (`parent` field), set state (PATCH `{"state": id}`), add and
  remove labels, add "blocked by" relations, add comment, create page —
  plus the small helpers tests and later phases need (create/list project,
  list states/labels, list pages).
- Test strategy: **live test against a per-run scratch project** (the
  acceptance's first option, chosen because the live board is reachable):
  `TestLiveScratchProject` (`live_test.go`) creates `p3_test_<unix>` in the
  `jarvis` workspace, exercises every operation in it, and deletes it in
  `t.Cleanup` — the board is left exactly as found (re-verified after the
  run: only `Jarvis` + `ai_agent_build` remain). Credentials come from
  process env, falling back to `~/.config/jarvis/agent.env`; without them
  the test skips, so `go test ./...` stays green off this machine. A
  hermetic httptest suite (`plane_test.go`) models both API surfaces
  including the quirks below, so behavior stays pinned when the live board
  is unreachable.
- Verified API facts for this build (2026-10-31):
  - v1 list endpoints are paginated (`{"results": [...], "count": N}`);
    session-API lists (pages) return a bare array.
  - Work item field names: `state` (not `state_id`), `parent` (not
    `parent_id`). `labels` is a list of IDs and **PATCH replaces the whole
    list** — the client GETs, merges, and PATCHes the union for
    add/remove.
  - Relations: `POST .../work-items/<id>/relations/` with
    `{"relation_type": "blocked_by", "issues": [<work-item ids>]}` — the
    list field is named `issues`; singular per-type fields are rejected.
  - Comments take `comment_html` on create; `comment_text` is silently
    ignored (stored empty).
  - **Pages are session-API only**: there is no v1 pages route (404) and
    `X-Api-Key` on session routes 401s. Sign-in is form-encoded
    `POST /auth/sign-in/`; a successful sign-in **302-redirects to the
    instance's SITE_URL**, which on this deploy is the NAS LAN IP
    (192.168.1.50) — unreachable from the AI box. The client therefore
    never follows redirects on sign-in and treats 2xx/3xx + a session
    `Set-Cookie` as success.
  - Session cookie is `session-id`; the CSRF token must be re-fetched
    after login (it moves into the session).

## P4 (2026-10-31) — plane-bridge service (webhooks + catch-up)

- Service: `cmd/plane-bridge` + `internal/bridge` (core) — receives Plane
  webhooks, verifies the HMAC-SHA256 `X-Plane-Signature` (hex over the raw
  body, secret per webhook), ignores events whose actor is the Jarvis bot
  (`PLANE_BOT_EMAIL`, case-insensitive) so bridge and bot never loop, logs
  every event it acts on to a JSONL events file, and enqueues agent jobs
  for items in a trigger state: `plan_epic` (item with `epic` + a
  `plan:<model>` label still in Backlog) and `run_task` (sub-item with a
  parent in Ready, no job yet). Job ids are `plane-<trigger>-<item.ID>`,
  enqueue idempotent.
- **Catch-up sync on every start** re-runs the same trigger check over the
  whole watched project, so changes made while the bridge was down are
  picked up at boot. Trigger checks are actor-agnostic by design.
- The trigger check also runs in the background on every `issue` webhook
  for the watched project; the HTTP handler always answers 200 fast (bad
  signature → 403, both logged).
- systemd: `systemd/plane-bridge.service` (`/usr/local/bin/plane-bridge`,
  `EnvironmentFile=/home/agent/.config/jarvis/agent.env`, restart
  on-failure). Installed and enabled 2026-10-31: healthz 200 on :8102,
  watching `AAB`, webhook registered for `http://111.1.2.27:8102/webhook/plane`.
- Env: `PLANE_URL`, `PLANE_API_KEY` (bot key), `PLANE_BOT_EMAIL`,
  `PLANE_BOT_PASSWORD` (the session used for webhook management),
  `PLANE_WORKSPACE` (default `jarvis`), `PLANE_PROJECT` (default `AAB`),
  `BRIDGE_LISTEN` (default `:8102`), `BRIDGE_WEBHOOK_URL` (default
  auto: `http://<lan-ip>:<port>/webhook/plane`), `BRIDGE_SECRET_FILE`
  (default `~/.config/jarvis/plane-webhook-secret`, 0600),
  `BRIDGE_EVENTS_FILE` (default
  `~/.local/share/jarvis/plane-bridge/events.jsonl`).
- **Webhook payload shape (verified byte-exact from a live delivery, via
  tcpdump + HMAC against the stored secret):** `data.state` is an OBJECT
  `{"id","name","group"}` and `data.labels` is a list of OBJECTS
  `{"id","name","color"}` — not bare ids; the project reference is
  **`"project"`, not `"project_id"`**; `action` values are
  `created`/`updated`/`deleted`; the actor sits under
  `activity.actor.email`. The first version of the bridge's `Data` struct
  assumed the bare-id shape and failed `json.Unmarshal` on every real
  event — the handler answered 400 **without logging**, which is why the
  live test looked like "no delivery" until a pcap on :8193 showed the
  POSTs arriving and being rejected. Lesson: the "bad payload" path now
  logs a warning; delivery latency on this NAS is ~100 ms, so a missing
  event is a parse/verify problem, not a transport one.
- Webhook management (list/create/retrieve/regenerate/delete) is
  **session-API only** (`/api/workspaces/<slug>/webhooks/`, workspace
  ADMIN) — there are no webhook routes on the v1 external API. The API
  reveals the `secret_key` only on create and regenerate, so the bridge
  stores it in `BRIDGE_SECRET_FILE` and treats EnsureWebhook as
  idempotent (URL match → no-op).
- **Membership reality (blocked the "human edit" acceptance half):**
  editing a work item (v1 PATCH) requires PROJECT membership; adding a
  project member requires WORKSPACE membership
  (`POST .../projects/<id>/members/` → 400 "Member not found in workspace"
  for non-members). `nathan219@gmail.com` is a PENDING invite on workspace
  `jarvis` (created by P2) that only a human can accept in the Plane UI —
  the invite token is not exposed via any API, so headless member-add is
  impossible. The live test detects membership via
  `plane.ListWorkspaceMembers` (new, v1) and runs its human-edit
  assertions only when the admin is a member; accepting the invite in the
  UI enables them automatically on the next run.
- Live test (`TestLiveWebhookAndCatchUp`): per-run scratch project
  `p4_live_<rand>` in workspace `jarvis`, cleaned up in `t.Cleanup`.
  Headless half: bot events (create + label add on an epic in Backlog)
  must be delivered, signature-verified and **ignored** (the
  "webhook ignored (bot event)" log line is the live delivery proof; no
  event line, no job). Stop-window half: an epic made while the bridge is
  stopped must be queued by the catch-up sync after restart. Skips when
  the Plane env is unavailable, so `go test ./...` stays green elsewhere.
  Passes in ~17 s (2026-10-31).

## P5 (2026-11-01) — old board migrated to Plane, old tool retired

- Tool: `cmd/board-migrate` (one-shot Go, re-runnable). Reads the old
  board via `internal/kanban`, writes through the `internal/plane`
  client, and ends with a live recount that exits non-zero unless the
  open item counts match. Flags: `-dry-run`, `-verify-only`,
  `-board-dir`.
- **Definition of "open" (the old board has no epic status and no
  closed state):** every epic counts as open (all 8 migrated), open
  task = status != done (52 of 53; the one done task, P1, skipped).
  Three epics have no tasks at all ("Test" scratch epic, "Make new
  interactive queue dashboard", "Full e2e test and fix of voice chat")
  — migrated as-is; close them on Plane if they're stale.
- Mapping: epic -> Backlog work item with the `epic` label; task ->
  sub-item under its epic (52/52 have a parent). Old status -> state:
  backlog -> Backlog; in-progress and review -> **In Progress** (the
  new board's "Plan review" column is for plan approval, not code
  review). Title/description/content carried over; old status,
  priority and model recorded in the description.
- **Idempotency:** every migrated item carries a marker line in its
  description — `migrated-from: old-board epic=<uuid>` /
  `task=<uuid> epic=<uuid>` — a re-run matches markers and skips
  existing items. Evidence: run 1 created 38 then hit a 429
  (Plane rate-limits v1 writes — the tool now backs off 30 s on 429,
  up to 10 attempts), run 2 created 22 / skipped 38, run 3
  `created=0 skippedExisting=60`.
- Acceptance evidence (recount + independent curl against the live
  project `AAB`, id `c9336196-3024-4477-a44e-8776df1ff24b`):
  **60/60 items** — 8 with the epic marker, 52 with the task marker;
  52/52 sub-items have a parent; sample checks: NoBrakes epic in
  Backlog with the epic label, the one review task
  (5ee2c959 "Bench service skeleton") in In Progress with its marker
  and old metadata intact.
- **What was disabled (acceptance: old tool no longer starts on boot):**
  `jarvis-dashboard.service` (the old board UI on :8080). It was
  *already disabled* (no wants symlink; the journal is truncated so the
  disable time is unknown), but a running instance was crash-looping —
  :8080 is held by the t2i-bench docker container, so the dashboard
  could not bind. This round: `systemctl stop jarvis-dashboard`
  (now inactive) and the unit's documented inhibit file
  `/var/lib/jarvis/dashboard.disabled` touched as a second lock. The
  unit file (repo + /etc copy) carries a RETIRED header.
- Note: the dashboard also rendered the queue/agent-job views — those
  are gone with it; the queue is visible via `jarvis-jobs` and Grafana.
- Stale statuses to tidy on Plane (migrated faithfully, not edited):
  on "Work board: Plane + Plan with model" cards P2 (#121), P3 (#122),
  P4 (#123) are already merged; on "T2I image generator benchmark" card
  5ee2c959 is PR #117 (merged). Close them when reviewing the board.
- Open decision (PLAN.md): history migration — **open items only, per
  this job's instruction**; the old board's data stays untouched on disk
  (~/.local/share/jarvis/kanban), so a later history migration is still
  possible if wanted.

## P6 (2026-10-31) — model-prefixed comments + Discord board links

- CLI: `cmd/jarvis-board` — `link <item>`, `item <item>` (one line:
  AAB-n [state] title link), `comment <item> [msg...]` posts
  `<model>: <msg>` as `comment_html` (one `<p>`, HTML-escaped, newlines
  to `<br>`; model from `--model` → `$JARVIS_MODEL` → "jarvis-brain").
  Items are addressed by UUID, `AAB-n` or bare `n`; the project from
  `--project` → `$PLANE_PROJECT` → "AAB". Config via `plane.FromEnv()`
  (PLANE_* env, falling back to ~/.config/jarvis/agent.env). Flags come
  BEFORE the positional args (Go flag convention, like jarvis-discord
  ask); a guard fails loudly when a flag is typed after them, because
  it would otherwise be posted inside the comment text — hit exactly
  that in live testing (first scratch comment AAB-61 contained
  "--model jarvis-brain"; the item was deleted and recreated as AAB-62
  with a clean comment).
- `jarvis-discord notify --item AAB-n` appends the item's board link on
  its own line (same guard for `--item` after the message).
- `internal/agentjob`: every round's env now carries
  `JARVIS_MODEL=<ModelFor(driver)>` (stale inherited value stripped
  first), so a round knows its model name for the prefix.
- `internal/plane` additions: `FromEnv`/`envValue` (per-call env-file
  fallback — deliberately no sync.Once, so tests can point PLANE_ENV_FILE
  at a temp file), `GetProject`, `ProjectByRef` (UUID → single GET;
  identifier → list + match), `WorkItemByRef` (UUID fast path; display
  id → ListWorkItems + sequence match, since v1 GETs only accept UUIDs),
  `ItemLink`, `ListComments`. httptest coverage for all five in
  plane_test.go (fake gained GET /projects/<id>/ and a paginated
  comments list).
- **Board link format (verified live, not guessed):**
  `<base>/<workspace>/browse/<identifier>-<sequence>`, e.g.
  `http://111.1.2.222:8088/jarvis/browse/AAB-43`. Evidence: the live
  SPA route manifest defines `:workspaceSlug/browse/:workItem`, and the
  issue-detail page chunk redirects to
  `/${workspaceSlug}/browse/${project_identifier}-${sequence_id}`. The
  older `.../projects/<projectId>/issues/<issueId>` route still exists
  but client-side redirects to the browse URL.
- Acceptance evidence (2026-10-31, live board): scratch item AAB-62
  (d7b5c1a3-de0e-4baa-8238-6167f8e44b93) in AAB carries the comment
  `<p>jarvis-brain: P6 acceptance: ...</p>` (verified via the raw v1
  comments API, not the client); AAB-43 (this job's card) carries a
  `jarvis-brain:` comment; a Discord status message was sent with
  `jarvis-discord notify --item AAB-62` and the appended link line
  `http://111.1.2.222:8088/jarvis/browse/AAB-62`.
- Scope note: state writeback remains C1's job — P6 does not move
  items. AAB-62 is left in Backlog as the acceptance artifact (title
  says "safe to delete").
- `internal/bridge` live test fix (found by the P6 full-suite run):
  the admin had accepted the workspace invite since P4, so
  TestLiveWebhookAndCatchUp's human-edit half now runs — and it 403'd
  on the admin's work-item rename. On this Plane build a plain member
  add grants the default viewer role (5); editors are **role 20**
  (probe: role choices 10/30/40 and the names "editor"/"admin" are all
  rejected, 20 works). The test now adds the admin with
  `{"member": id, "role": 20}`; the half passed live (rename 200,
  event logged, plan job queued).

## Board reorg (2026-10-31) — one project per epic

- **Nathan's decision** (via `jarvis-discord ask`, job
  chat-1793504779-d19c): "One project per epic. Reorganize everything.
  Make that the standard going forward. Add it to you Claude.md or
  wherever it should go." Original question: organize the board with
  Plane projects like epics, or was there a free-plan limit against it?
- **No limits to worry about:** self-hosted Plane CE — the cloud
  "free plan" does not apply, and Plane has no project-count cap
  anyway. Projects are the native unit; there are no epics in Plane
  (a parent work item stands in — that is the epic item, label
  `epic`).
- **The standard:** each epic = one Plane project with a short
  identifier; the epic is the project's epic work item and its tasks
  are work items in the same project, parented to it. `AAB`
  (ai_agent_build) is the legacy single project — it keeps only
  AAB-62 (the P6 acceptance artifact). New epics: `plane-epic create
  "<title>" --identifier XXX [--description]` — scaffolds the
  standard states, the epic label and workspace members as editors,
  creates the epic item, appends the identifier to `PLANE_PROJECTS`
  in `~/.config/jarvis/agent.env` and restarts `plane-bridge`. That
  is the only supported way to add a project.
- **Migration (plane-migrate-projects):** source of truth = the 60
  marker-tagged items in AAB (8 epics + 52 tasks; markers
  `migrated-from: old-board epic=<uuid>` / `task=<uuid> epic=<uuid>`
  in `description_html`, P5). Each item is recreated in its epic's
  project carrying a `split-from: AAB-<seq>` twin marker; states are
  mapped case-insensitively by name (handles AAB's "In Progress");
  tasks get the new epic item as parent. Run in two passes:
  `-no-delete` (create + verify), then a second pass that
  hard-deletes the old AAB items — the delete gate refuses unless the
  item is marker-tagged AND its twin exists (idempotent; AAB-62 has
  no marker and is untouched either way).
- **Project identifiers:** E2E (Full e2e test and fix of voice
  chat), TST (Test), NBR (NoBrakes), MOE (vram cache benchmarking of
  MoE models), QDB (Make new interactive queue dashboard), VCH (Own
  voice channel), T2I (T2I image generator benchmark), WBD (Work
  board Plane Plan with model).
- **Plane project-name charset (probed live, create+delete):**
  REJECTED with 400 "Project name cannot contains special
  characters.": `. : + ( ) & - , @ ! # % * = ? ; ^ { } |`. ALLOWED:
  letters, digits, space and `/ _ ' "` and a few others. Consistent
  with P2's FORBIDDEN_IDENTIFIER_CHARS note (hyphen is rejected too).
  Work-item names have no such restriction — only project names.
  `planeepic.SanitizeProjectName` implements the allowlist (anything
  else becomes a space, runs collapsed); `EnsureProject` sanitizes
  before lookup and create, so lookups match the stored name.
- **New projects auto-create 5 default states** (Backlog, Todo,
  In Progress, Cancelled, Done) — a freshly scaffolded board shows
  10 states until the extras are gone. `planeepic.EnsureBoard` now
  prunes non-standard states that no work item references (same rule
  as scripts/plane-setup.sh), after creating the standard set.
- **Rate limit:** Plane 429s bursty v1 traffic
  (`{"error_code":5900,"error_message":"RATE_LIMIT_EXCEEDED"}`) — on
  reads and writes alike (hit live mid-migration on a state create AND
  a states list). Backoff is therefore in the client itself:
  `plane.Client.do`/`doList` retry a 429 with a 30 s pause, up to 10
  attempts, ctx-aware, so every call in every tool rides out a throttle
  window. (An earlier design wrapped individual write calls in
  `plane.WithBackoff`; that was removed when the retry moved down.)
- **Multi-project bridge:** `plane-bridge` watches every project in
  `PLANE_PROJECTS` (comma-separated; falls back to `PLANE_PROJECT`,
  then `AAB`). Webhooks are workspace-level, so `HandleWebhook` gates
  on the event's project being watched (`isWatched`) and `CatchUp`
  loops all watched projects. Bot-actor events are still ignored
  (`isBot`), so the migration — which acts with the bot key — queued
  zero bridge jobs.
- **Prefixed item refs:** `T2I-3` resolves its own project by
  identifier prefix (case-insensitive) via `plane.ResolveItemRef`;
  UUIDs and bare sequences fall back to `--project` /
  `$PLANE_PROJECT` / `AAB`. Used by `jarvis-board` and
  `jarvis-discord notify --item`, so no `--project` flag is needed for
  prefixed refs.
- **Deleting a parent work item cascade-deletes its children.**
  Pass 2 deleted the 8 epic items and the 52 tasks vanished with
  them (the AAB-9 delete then 404'd "The requested resource does not
  exist."). The delete gate therefore treats a 404 as already-gone
  (log and continue), which also makes re-runs idempotent.
- Verification: pass 1 (`-no-delete`) — `done: 60 created, 0 skipped
  (twins), 0 deleted, 1 untouched`, exit 0; per-project verify E2E 1,
  TST 1, NBR 7, MOE 5, QDB 1, VCH 13, T2I 9, WBD 23. Pass 2 — the 8
  epics deleted explicitly, the 52 tasks gone by cascade. AAB was left
  with exactly one item: AAB-62 (untouched, unmarked). Independent
  curl recount against the v1 API: 8 projects, 60 items total, each
  project exactly 1 epic (null parent) + its tasks (all parented to
  it), every item carrying a `split-from: AAB-<seq>` marker. Idempotent
  re-run: `done: 0 created, 0 skipped (twins), 0 deleted, 1
  untouched`, exit 0. New bridge (8 projects in `PLANE_PROJECTS`)
  restarted: startup catch-up `projects: 8, items: 60, queued: 0`,
  `/healthz` ok.

## One project per epic — enforcement check (2026-11-01)

- Job chat-1793508833-9f69 asked to enforce the standard. Verified it is
  already enforced end-to-end; no code change was needed (the standard and
  the migration shipped in PR #128, job chat-1793504779-d19c). What was
  added this round is the enforcement wording in `CLAUDE.md` + this note,
  so future implementations follow the rule:
  - **Convention:** `CLAUDE.md` "Work board (Plane)" + this file's "Board
    reorg" section state the rule — one project per epic, the project named
    after the epic, every task in that project and parented to the epic.
  - **Single creation path:** `plane-epic create` is the only supported way
    to add a project. `planeepic.EnsureProject` resolves an existing project
    by name (case-insensitive, sanitized) or identifier before creating, so
    a re-run — or a second run with a different identifier for the same
    title — cannot create a second project for the same epic. The epic item
    is created in that project and its identifier is registered in
    `PLANE_PROJECTS` (bridge restarted to watch it).
  - **No task-sync script exists yet.** Task sub-items are created by humans
    in the Plane UI (inside the epic's project) and, when Phase B4 ("create
    the tasks as sub-items in Plan review") is built, by it. B4 is not built;
    when it is, it must create sub-items in the epic's project —
    `plane.Client.CreateWorkItem(projectID, WorkItemCreate{Parent: epicID})`
    with `projectID` the epic item's project. The one-time migrations
    (`board-migrate`, `plane-migrate-projects`) already ran and are
    idempotent/retired; they are not recurring syncs.

## A3 — sync planner models to Plane plan: labels (2026-11-01)

- Job plan-epic-planner-plan-with-m-1793259204-9. One `plan:<model-id>`
  label per enabled planner model, created when enabled, removed when
  disabled; runs at bridge start and on every change to
  `config/models.yaml`. New package `internal/planlabels` (Sync +
  Watcher), `plane.Client.DeleteLabel`, watcher wired in
  `cmd/plane-bridge/main.go` (`BRIDGE_MODELS_FILE`, default
  `/home/agent/ai-agent-build/config/models.yaml`, 15 s poll).
- **Labels are per-project** in Plane, so Sync runs over the bridge's
  watched projects (`PLANE_PROJECTS`). New epic projects are covered
  because `plane-epic create` restarts the bridge, which re-syncs.
- **Polling, not fsnotify:** no fsnotify dependency in go.mod; mtime+size
  poll at 15 s is enough (label sync is not latency-critical).
- **Staggered start:** the watcher's first sync waits for the startup
  catch-up to finish. Plane rate-limits the shared bot key (429, 30 s
  pauses in `plane.Client`), and both bursts at boot pushed the key over
  the limit — the live bridge test's restarted run sat in the 429 retry
  loop and timed out. After staggering, that test passes in ~30 s.
- **Watcher retry semantics:** a failed sync retries on the next tick
  even without a file change (covers Plane outages at boot); a missing or
  unparseable file keeps the last good state applied and does not abort
  the watch.
- **Live-test lock:** `go test ./...` runs packages in parallel, and the
  live tests in `internal/plane`, `internal/bridge` and
  `internal/planlabels` share the bot API key — their bursts 429 each
  other into the tests' timeouts (seen once in a full run).
  `plane.AcquireLiveLock` (file lock in /tmp, stale after 10 min)
  serializes them; a test skips if the lock is still held after 5 min.
- Sync only touches `plan:`-prefixed labels; foreign labels (epic, ...)
  are left alone, verified hermetically and on the live scratch project.

## A6 — per-location queues and workers (2026-11-01)

- Job plan-epic-planner-plan-with-m-1793259204-12. `jobs.gpu`, `jobs.cpu`,
  `jobs.igpu` queues on the existing `tasks` topic exchange (bound `q +
  ".#"` like the priority queues), each consumed by its own
  `queue.LocationWorker` — started by `cmd/worker` via
  `queue.LocationWorkers`, one per queue.
- **Dedicated AMQP channel per worker** (`Conn.NewChannel`): the main
  consumer shares one channel, so its `Qos(1,0)` prefetch is one delivery
  slot across all four priority queues. A location worker on its own
  channel with prefetch=1 is what makes per-location parallelism real —
  a long GPU job cannot hold the CPU/iGPU queues' delivery slots.
- **Pause is per queue**: `Pause()` holds an arriving delivery unacked;
  with prefetch=1 on the worker's own channel that holding is what stops
  the queue. The game watchdog (streamwatcher in `cmd/worker`) pauses
  only `jobs.gpu` on stream start and resumes it on stream end; CPU and
  iGPU keep running.
- Phase A handler in the worker logs + acks; the model-swap / cloud
  dispatch that runs a location job is B2/C1's job. Queues added to
  `rabbitmq/definitions.json` (no x-max-priority, bound like the others).
- **Tests:** `LocationQueueFor` + pause-routing are hermetic;
  `TestLocationWorkers_CPUFinishesWhileGPURunning` (the acceptance
  check) and `TestLocationWorkers_PausedGPUQueueDoesNotBlockCPU` run
  against the live broker and skip when it is unreachable. They use
  per-run `a6test.*` queues so their routing keys never match the real
  `jobs.*` bindings (no race with the deployed worker's location
  consumers).
- Follow-up (not A6): the dashboard's queue view still lists only
  voice/coding/background; add the `jobs.*` queues when the queue view
  is next touched.

## B2 — planning worker handler (2026-11-02)

- Job plan-epic-planner-plan-with-m-1793259204-14. New
  `internal/planworker`: `Parse` (the B1 instructions contract),
  `Handler.Run` (dispatch), `Engine` + `ClientEngine` (one client per
  registry endpoint, 30-min planning timeout — `models.NewClientWithBaseTimeout`,
  the chat default of 120 s is far too short for a 27B plan), `Cloud` +
  `API` (anthropic Messages API with thinking budget from the registry's
  `thinking: "thinking.budget_tokens:N"`, openai-compatible chat
  completions), `Planner` + `BasicPlanner` (B2 default: one completion;
  B3 replaces it with the Plan/Decompose/Review pipeline).
- **Swap semantics** (the loading rule): a local planner that is not
  loaded unloads the current work model, loads the planner, and the
  handle's `Release` unloads the planner and reloads the previous model
  — Release runs even when the plan fails, and a failed load restores
  the previous model before the error surfaces. The front-end model
  (`FrontEnd`, default `jarvis-igpu.gguf`) is never the unload target.
  Already-loaded planner → no swap at all. Cloud planner → engine is
  never touched.
- **Worker wiring** (`cmd/worker`): a `KindAgent` message whose
  instructions `Parse` as a plan job runs in `handlePlan` instead of
  being left for the agent-run loop; runs are serialized (`planMu` — one
  big model fits in VRAM). Result goes to history + metrics + HA notify,
  the plan document to the job's `plan.md`, and the agentjob store entry
  is resolved to done/failed so the loop never claims a plan job (B1
  ingests it as ready). No registry → loud log, plan jobs fall back to
  the agent-run loop as before B2.
- **Container reality:** the worker image carries only the binary, so
  the registry is mounted read-only (`/config:ro` in compose,
  `WORKER_MODELS_FILE=/config/models.yaml`) — model changes need no
  rebuild. The registry's host-local endpoints (localhost:11434) are not
  reachable in-container, so `ENGINE_URL`/`OLLAMA_URL` override them for
  the engine clients (same precedence as `models.NewClient`).
- **Naming convention (open for D1):** the registry id is the name the
  worker calls a local model by. Today the engines report GGUF base
  names (`jarvis-brain.gguf`, `jarvis-igpu.gguf`) and ollama is
  inactive — a plan job for `qwen3.8-27b` cannot truly swap until the
  planner model is on an engine serving it under its registry id (Phase
  D). Until then the cloud planner (`claude-sonnet-5-5`, enabled) is the
  one that really runs.
- **Tests:** all hermetic. Handler runs against a stub engine that
  records the exact op sequence — the acceptance checks:
  `TestRunLocalSwap` (unload brain → load planner → chat → unload
  planner → load brain, front-end untouched), restore-on-load-failure
  (both failure orders), restore-on-plan-failure, already-loaded
  (no swap), cloud (engine never touched). `TestClientEngineOllama` /
  `TestClientEngineLlamaCPP` drive the adapter against fake engines in
  both API dialects; `cloud_test.go` checks the request/response shapes
  of both cloud APIs via httptest.

## B3 — planning prompts + task schema + pipeline (2026-11-02)

- PR #136. `prompts/plan.md`, `decompose.md`, `review.md` ({{TOKEN}}
  templates) plus `prompts/task.schema.json` (draft-07: task ids
  `[a-z][a-z0-9-]*`, S/M sizes, runnable acceptance checks, executor
  drawn from the registry). `internal/planworker`: `TaskSchema`
  (jsonschema plus cross-task rules, with location-bearing error text)
  and `Pipeline`, which replaced the B2 `BasicPlanner`: Plan (epic +
  repo summary → PLAN.md) → Decompose (same model; an invalid JSON
  reply is retried up to 2 times with the parse error, then the job
  fails) → Review (a different model when `Pipeline.Reviewer` is set
  and differs from the planner — otherwise skipped; its fixes are
  applied once, and a bad review keeps the decomposed list).
- Worker wiring: the worker stores `tasks.json` next to `plan.md`;
  compose mounts `prompts/` and the repo read-only
  (`WORKER_PROMPTS_DIR`, `WORKER_REPO_ROOT`); the bridge carries the
  epic description (HTML stripped) in the job. The reviewer is left
  unset in the worker for now (cloud review spend cap is an open
  decision).

## B4 — deliver the plan to the board (page + sub-items) (2026-11-02)

- Job plan-epic-planner-plan-with-m-1793259204-16.
  `planworker.Deliverer` (`internal/planworker/deliver.go`): the plan
  document becomes a project page named `Plan: <epic name>` whose body
  is the plan in a `<pre>` block (it is Markdown and go.mod has no md
  library — the page editor takes HTML, so it is escaped and rendered
  verbatim); the epic's description gets a `plan-page: <url>` line; a
  model-prefixed "Plan ready" summary comment goes on the epic; each
  task becomes a sub-item of the epic in **Plan review** whose first
  description line is `plan-task: <task-id>`, with a blocked-by
  relation per dependency and one `model:<executor>` label (created in
  `placement.ModelColor` if the project does not have it) — never a
  `run:` label, placement comes from the registry (A5). The epic moves
  to Plan review only after everything else succeeded — that state is
  the "ready for approval" signal B5 acts on.
- **Plane page quirks (verified live):** the create input field is
  `description_html` — `content_html` is silently ignored (the page is
  created empty). The page list endpoint omits the body entirely; only
  the single-page GET returns it → `plane.GetPage` added, plus
  `Client.PageLink` (`/{ws}/projects/{projectUUID}/pages/{pageUUID}`,
  the format the UI uses). The plane_test fake now models the
  list-omits-body quirk so a regression is caught.
- **Convergence:** re-delivery reuses sub-items (via the `plan-task:`
  marker) and the page (by name), re-asserts state/labels and replaces
  the plan-page line instead of appending — a partially failed
  delivery can be retried without duplicating cards. The summary
  comment is the only append-only artifact on re-delivery (accepted).
- **Label gating:** an executor model that is not an enabled executor
  in the registry fails the whole delivery — `placement.Sync` would
  delete the label again and C1 could not route a job to it.
- **Worker wiring** (`cmd/worker`): the Deliverer is built from
  `plane.FromEnv()` whenever the plan handler is (the worker
  container has the PLANE_* env from agent.env); a delivery failure
  records the job failed through the normal fail path (the plan is
  already stored as plan.md), and missing board credentials make plan
  jobs fail on delivery with a loud log line.
- **B5 caveat:** page reuse is by name; a re-plan with different
  content needs an UpdatePage/DeletePage, which the client does not
  have yet.

## B5 — approval and re-plan (2026-11-02)

- Job plan-epic-planner-plan-with-m-1793259204-17. Two bridge
  transitions, both on epics that already carry a delivered plan (the
  `plan-page:` line in the description is the "has a plan" marker):
  - **Plan approved → Ready.** `approveSubItems` moves every
    Plan-review sub-item to **Ready** and queues a `run_task` job for
    each. Idempotent on re-check (a second pass finds nothing left in
    Plan review). run_task jobs go to the store only — the PlanQueue
    is for planning, so approval publishes nothing to RabbitMQ.
  - **Planning + has a plan → re-plan.** `enqueueReplan` collects the
    epic's comments, queues a `replan_epic` job (deterministic id
    `plane-replan_epic-<id>`) and publishes it to the PlanQueue at the
    same front-of-queue priority as a first plan. A plain move back to
    Planning *without* a delivered plan is a no-op — it is just
    "start planning", which the original `plan_epic` label trigger
    already covers.
- **Comment filtering:** `replanComments` lists the epic's comments,
  strips HTML, and drops the ones authored by the bot (matched by
  member id, resolved from `cfg.BotEmail` via `ListWorkspaceMembers`
  and cached in `b.botMember`). So the re-plan prompt carries only
  human feedback — not the bot's own "Plan ready" / position notes.
  The bot's id is resolved lazily on the first re-plan; if it cannot be
  resolved the comments are kept unfiltered (a missing filter must not
  block a re-plan).
- **`Store.Refresh`** (new, `internal/agentjob`): re-queues a *finished*
  job (Done/Failed/Skipped) in place with new instructions and puts it
  back to Ready. An in-flight or missing job is refused ("not
  finished") so a live re-plan never clobbers a running one. The
  deterministic id means a re-trigger of the same epic lands on the
  same job record.
- **Planworker side:** `Parse` accepts `replan_epic` as well as
  `plan_epic`; `Job.Comments` carries the human feedback and the
  planning prompt gains a "Comments (feedback):" block (rendered as
  "(no comments)" when empty) — see `prompts/plan.md`. The Deliverer's
  re-plan path reuses the page (updates its body via `UpdatePage`) and
  the kept cards (in place, via `SetDescriptionHTML`), and deletes the
  cards the new plan dropped — but only when they are still in Plan
  review or Ready; a card someone already moved to work is left alone.
- **Live-verified this round:** the page body update (session PATCH)
  and work-item deletion (v1 `DELETE`) — the two client methods B4
  flagged as not yet exercised. The B5 acceptance
  (`internal/bridge/b5_live_test.go`) runs the whole arc on a scratch
  project: deliver a 2-task plan → approve (cards to Ready, run_task
  jobs, no queue publish) → comment + move to Planning (re-plan job
  carries the human comment, bot's filtered) → deliver the re-plan
  (page updated, kept card refreshed, dropped card deleted, new card
  created, single `plan-page:` line kept).
- **Plane quirk (verified live):** commenting requires project
  membership with at least the editor role — a plain member add grants
  the viewer role (5), which gets a 403 on comment POST. The live test
  adds the admin to the scratch project as editor (role 20) first,
  mirroring `live_test.go`.

## C1 — run_task execution loop + task-runner service (2026-11-03)

- **Harness choice: Aider** (not OpenHands). Aider is a single Python CLI
  pointed at any OpenAI-compatible endpoint (`--openai-api-base`) that
  edits files on disk; OpenHands is a full agent server (runtime, browser,
  event loop) — more moving parts for a loop where the runner already owns
  git, the acceptance check, retries and board state. Aider runs from a
  venv at `~/.local/share/task-runner/venv` (CPython 3.12 — aider does not
  install on the system's Python 3.14) and the runner execs its `aider`
  binary directly (`internal/taskrunner/harness.go`).
- **Endpoint/model resolution:** the local engines serve the GGUF file
  path, not the registry id, as the model name, so the harness asks the
  endpoint's `/v1/models` and requires exactly one model — the runner must
  know which model it is paying the box's VRAM for. The default endpoint is
  `http://localhost:11434/v1` (the brain — the registry's only executor at
  C1 time; a second local executor with a different endpoint needs the
  `Aider.Endpoint` field set per model).
- **Host service, not a container:** `cmd/task-runner`
  (`systemd/task-runner.service`, `/usr/local/bin/task-runner`) is the
  only consumer of the location queues (`jobs.gpu`/`jobs.cpu`/`jobs.igpu`).
  The worker's placeholder silent-ack location workers were removed in the
  same round — they would have been competing consumers that ack and drop
  run_task jobs. The loop runs on the host because the worker container
  mounts the repo read-only and carries no git, Go toolchain or Python.
- **Store settling:** a run_task result settles its agent-job entry —
  Running while in flight; Passed and Blocked → Done (Blocked is settled:
  a human moves the card); Waiting → Skipped, the terminal "settled
  without a verdict" state, so the bridge's next catch-up re-triggers the
  job while the card is still Ready; infrastructure error → Failed (the
  bridge re-triggers on a terminal entry + card state, self-healing). A
  missing entry (the bridge ingest has not landed) is created from the
  queue payload first, so the store stays whole.
- **Game watchdog on the lane:** while a Sunshine/Steam stream is active
  (endpoints set), the GPU lane is paused — GPU jobs wait in the queue
  instead of fighting the game for the 4090.
- **Branch shape:** the epic branch is `epics/<PREFIX-<epic seq>>` — the
  task branch `epic-<PREFIX-<epic seq>>/task-<task seq>` must not sit under
  a ref named `epic-<code>`, so the epic branch gets the `epics/` prefix.
  Both live in `.worktrees/` worktrees (excluded from git status); the
  task worktree is dropped after the merge, the epic worktree is kept so
  later tasks of the same epic build on it and NOTES.md accumulates there.
- **Live-verified:** pending the toy-epic run (below).
## P6+ — `jarvis-board state`/`done`: worker updates the Plane item on task completion (2026-11-04)

- Job chat-1793781308-8f20 (Nathan): when the worker is done with a task
  it should update the Plane task; add that to tools and/or CLAUDE.md.
- **Tool:** `jarvis-board state <item> <state> [msg...]` moves the item
  to `<state>` (matched by name, case-insensitive, via
  `plane.StateByName` + `plane.SetState` — both already existed in P3)
  and, when a trailing message is given, posts it as a model-prefixed
  comment under the usual convention. `jarvis-board done <item> [msg...]`
  is the completion shortcut (state `Done`). Prints
  `<ref> -> <State> <link>`.
- **Test:** `TestLiveStateCommand` (`cmd/jarvis-board/state_test.go`)
  runs the built binary against a scratch project: state move
  (lowercase name), the `done` shortcut with comment (comment verified
  via `ListComments`), and an unknown state failing without moving the
  item. Board left as found (project deleted in defer).
- **CLAUDE.md:** the old rule "do not move item states from ad-hoc
  rounds — state writeback is owned by C1" is replaced by a standing
  instruction: after the PR is open and Discord is notified, the worker
  moves the item to `Done` with a comment carrying the outcome and PR
  link; a failed/stuck job goes to `Blocked` with the reason. The
  worker owns these transitions until C1 writes states back
  automatically.
- Binary reinstalled to `/usr/local/bin/jarvis-board` (root-owned;
  `sudo install` after `go build`).

## Broker-restart survival: bridge publishers + location workers reconnect (2026-11-06)

- **Found (job chat-1793835565-142a):** the RabbitMQ container restarted
  ~12:20 Nov 5 while both consumers of the queues were up.
  - `plane-bridge` had wired its publishers once at start and `IsClosed`
    only reports a Close *we* initiated, so after the broker came back it
    still held no connection: the TST-3 run job re-triggered and failed
    every 5-minute catch-up with "no location queue publisher (broker
    down at bridge start)" until a bridge restart.
  - `task-runner`'s `LocationWorker.consumeLoop` returned when the
    deliveries channel closed (the dead broker connection), so the
    workers went silent forever; the republished TST-3 message sat in
    `jobs.cpu` unwatched.
- **Bridge fix (46f6ac6):** the publishers now sit behind a `queueRelay`
  (mutex-guarded holder of the `bridge` publisher interfaces). A failed
  start runs a 30 s connect retry; a live one starts a `watchConn`
  watchdog that calls `Conn.Reconnect` while the new `Conn.Dead` reports
  the underlying AMQP connection is gone. A publish racing the swap
  errors once; store-settle + catch-up re-trigger retries it. Unit test:
  `cmd/plane-bridge/main_test.go` (nil relay fails with the store-settle
  error; swap changes what publishes see next; interface conformance).
- **Runner fix:** `LocationWorker.consumeLoop` now re-subscribes when the
  deliveries channel closes and the worker is still running —
  `Conn.Reconnect`, then re-declare/bind/QoS/Consume on a fresh channel,
  5 s backoff while the broker is down. The pause flag is untouched, so a
  game-watchdog GPU pause survives a broker restart. A `Stop` still wins:
  the running check happens before the resubscribe.
- **Live-verified:** both binaries deployed and the services restarted.
  02:57 — restarted bridge republished TST-3 to `jobs.cpu` on the startup
  catch-up; 03:03 — the new task-runner picked it up, the TST-3 card
  moved to In Progress, and the run started (the C1 loop end-to-end, the
  first Plane task executed by the loop).
- TST-5: E2E: write the run_task marker note — done 2026-11-08 (commit 0b0309f)
