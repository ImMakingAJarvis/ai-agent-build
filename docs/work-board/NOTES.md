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
