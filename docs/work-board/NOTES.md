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
