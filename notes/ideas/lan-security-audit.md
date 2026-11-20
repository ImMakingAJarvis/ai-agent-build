# lan-security-audit — six LAN services are open, only one has a real login wall

Think job `think-lan-security-audit`, 2026-11-20. Verified live from the host
and against `111.1.2.27` (LAN IP): compose port blocks, env files (names only),
auth code paths, and curl/TCP probes of every candidate port.

## Findings

**The host firewall is off** (`sudo ufw status` → inactive). The LAN is one flat
`111.1.2.0/24` (box is `.27` wired, `.57` wifi; HA is `.200`, Plane is `.222`).
So "LAN surface" = every port bound without a `127.0.0.1:` prefix in
`docker-compose.yml`, plus anything bound on the host.

| Port | Service | Live probe via 111.1.2.27 | Auth | Where the credential lives |
|---|---|---|---|---|
| 8080 | dashboard | **200, no credentials asked** | ❌ none | `DASHBOARD_USER/PASS` exist in `~/.config/jarvis/local.env` but the dashboard container only loads `agent.env` (which has no such keys) → the Phase-6 basicAuth middleware (`internal/dashboard/dashboard.go:529`) never activates. `local.env` is also mode 644 |
| 3000 | Grafana | login wall, **`admin`/`admin` → 200** | ⚠️ default | no `GF_SECURITY_ADMIN_PASSWORD` anywhere; stock default persisted in the `grafana_data` volume |
| 9090 | Prometheus | open | ❌ none | n/a (no auth mechanism) |
| 8100 / 8443 | voice (STT→brain→TTS) | open, no challenge | ❌ none (intentional) | `VOICE_USER/PASS` in `local.env`, overridden by `/opt/jarvis-voice/auth-off.env` (empty = off, "until the HA-side integration sends credentials") |
| 9100 / 3001 | node- / nvidia-exporter | open | ❌ none | n/a — metrics only, no control |
| 1883 | RabbitMQ MQTT (for HA) | TCP open | ⚠️ weak default | `RABBITMQ_DEFAULT_USER/PASS` fall back to compose defaults `jarvis`/`jarvis-local` — no `.env` override exists, and `amqp://jarvis:jarvis-local@…` is hardcoded in the compose env of dashboard+ha. **The repo is public** (`gh api …/.private` → `false`), so these defaults are public knowledge |
| 8096 | Jellyfin | 302 → login | ✅ real login wall | Jellyfin's own DB (`/var/lib/jarvis/jellyfin/config`); readable copy in `~/.config/jarvis/jellyfin.env` (600) |
| 8120 | art (photo gallery + gen API) | down now (swap-window service) | ❌ none in code | n/a — `art_service.py` has no auth; bound 0.0.0.0 "meant to be opened from the LAN" |

**The engines are not a LAN surface.** `:11434`/`:11435`/`:11436` are
`127.0.0.1:`-bound and connection-refused from the LAN IP (verified). They are
auth-free only *inside* the docker bridge (any container can reach
`engine:11434` by name) — a container-compromise concern, not a LAN one.
Same story: redis `:6379`, RabbitMQ AMQP/management `:5672/15672/15692`,
memory `:8101`, bench `:8121`, resourced `:9104` (bridge-gateway-only).

**Plane `:8088` is not on this box** — it runs at `111.1.2.222` (separate
machine, same flat LAN; no plane container here). This box only holds its API
keys (`PLANE_*` in `agent.env`, 600). Plane's and HA's own surfaces belong to
their boxes but share the same exposure problem.

**Secrets concentration:** the dashboard and ha containers mount the whole
`~/.config/jarvis` dir — agent.env (ANTHROPIC_API_KEY, Plane keys), discord.env,
ha-token, jellyfin.env, plane-webhook-secret all readable from those two
containers. Fine while they run our code; a bad dependency or RCE in either
reads everything.

## Options / next steps

1. **Wire the dashboard's auth on** (highest value, ~zero work): the basicAuth
   code already exists — add `DASHBOARD_USER/PASS` to `agent.env` (or add
   `local.env` to the dashboard's `env_file`), restart, verify a 401.
   `:8080` currently exposes kanban (create/comment), job transcripts, Discord
   logs and the secrets page UI to anyone on the LAN.
2. **Set a real Grafana password**: `GF_SECURITY_ADMIN_PASSWORD` in the compose
   env (or change once in the UI). Kills `admin`/`admin` on the LAN.
3. **Loopback-bind the metrics trio** `:9090/:9100/:3001`: verified safe —
   Grafana's datasource is `http://prometheus:9090` and every Prometheus target
   is a container hostname, so no LAN client needs the host binds.
4. **Retire `jarvis`/`jarvis-local`**: real `RABBITMQ_USER/PASS` in a 600 env
   file, update the two hardcoded `amqp://` URLs and HA's MQTT credentials.
   Touches the HA box, so pair it with a HA-side change.
5. **ufw backstop, last**: default-deny incoming, allow-list what must stay
   open (8080, 8100/8443, 8096, 3000, 1883 from `.200`, 8120). Do it after
   1–4 so a wrong allow-list can't lock Nathan out of his own services.
6. **Hygiene (fold into the above)**: `chmod 600 local.env`; consider
   narrowing the `~/.config/jarvis` mounts in dashboard/ha; voice auth flips
   on when the HA integration is ready (already the plan); art `:8120` gets
   the same basicAuth as the dashboard when it next ships.

## Recommendation

Do **1 + 2 + 3 as one small PR** (env wiring + three compose port prefixes;
no UX change, every internal path verified container-name-based), then **4**
with its HA-side half, then **5** as the backstop. After that the only
unauthenticated LAN surfaces left are the metrics endpoints (gone, step 3),
Jellyfin (behind a real login) and the photo window's 8120 (short-lived,
step 6). Plane and HA boxes get the same treatment as separate follow-ups —
the flat-LAN + ufw-inactive pattern is a property of the whole household,
not just this box.
