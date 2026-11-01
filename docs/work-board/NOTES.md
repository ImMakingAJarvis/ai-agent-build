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
