#!/usr/bin/env bash
# plane-setup — idempotently create the Jarvis work board on the self-hosted
# Plane instance (P2 of docs/work-board/PLAN.md).
#
# Converges to, and verifies on every run:
#   * workspace "Jarvis" (slug jarvis), owned by the Jarvis bot user
#   * bot user $PLANE_BOT_EMAIL with an API token labeled "jarvis-bot"
#   * $PLANE_ADMIN_EMAIL as a workspace Admin (via a pending invite the human
#     accepts in the Plane UI — the invite-accept endpoint requires the
#     invitee's own session + one-time token, so it cannot be done headlessly)
#   * project "ai-agent-build" (identifier AAB)
#   * 8 states in plan order: Backlog, Planning, Plan review, Plan approved,
#     Ready, In progress, Blocked, Done
#   * "epic" label
#   * the env file ($PLANE_ENV_FILE) holding PLANE_API_KEY = the bot token
#
# First run: session bootstrap (bot sign-up, workspace create, token create),
# everything else through the v1 REST API (X-Api-Key). A second run is pure v1
# with the bot key — it creates nothing and changes nothing.
#
# Env file (default ~/.config/jarvis/agent.env) — never printed, never committed:
#   PLANE_URL            required — base URL of the Plane instance
#   PLANE_API_KEY        v1 API key; after a run this is the bot's token
#   PLANE_ADMIN_API_KEY  Nathan's original key, preserved as backup (first run)
#   PLANE_BOT_EMAIL      bot user email (default jarvis-bot@nathan.homes)
#   PLANE_BOT_PASSWORD   bot user password (generated on first run)
#   PLANE_ADMIN_EMAIL    admin user email (learned from users/me on first run)
set -euo pipefail

# --- configuration -----------------------------------------------------------
ENV_FILE="${PLANE_ENV_FILE:-${HOME}/.config/jarvis/agent.env}"
WS_NAME="Jarvis"
WS_SLUG="jarvis"
# Plane forbids hyphens in project names (Project.FORBIDDEN_IDENTIFIER_CHARS
# pattern), so the repo name maps to ai_agent_build; identifier AAB is the
# functional handle (work items are AAB-n).
PROJECT_NAME="ai_agent_build"
PROJECT_IDENTIFIER="AAB"
# jarvis-bot@ (no suffix) was created by an aborted first run whose password
# was lost — Plane has no password-reset without SMTP, so the bot is bot2.
DEFAULT_BOT_EMAIL="jarvis-bot2@nathan.homes"
DEFAULT_ADMIN_EMAIL="nathan219@gmail.com"
BOT_TOKEN_LABEL="jarvis-bot"
# Desired states as name|group|color, in display order. Sequence is
# (index+1)*1000 so the order survives re-runs.
DESIRED_STATES=(
  "Backlog|backlog|#94A3B8"
  "Planning|unstarted|#0EA5E9"
  "Plan review|unstarted|#6366F1"
  "Plan approved|unstarted|#8B5CF6"
  "Ready|unstarted|#22C55E"
  "In progress|started|#F59E0B"
  "Blocked|started|#EF4444"
  "Done|completed|#10B981"
)
EPIC_LABEL_COLOR="#6366F1"

# --- helpers -----------------------------------------------------------------
# log() writes to stderr so command substitutions (e.g. $(ensure_bot_token))
# only ever capture the value, never progress lines.
log() { echo "plane-setup: $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

mask() { # mask SECRET — first 10 + last 4 chars, for safe logging
  local s="$1"
  if [[ -z "$s" ]]; then printf '(unset)'
  elif [[ ${#s} -le 12 ]]; then printf '****'
  else printf '%s…%s' "${s:0:10}" "${s: -4}"
  fi
}

gen_password() { # 48 hex chars, alnum; no early-exiting pipe (pipefail-safe)
  od -v -An -tx1 -N24 /dev/urandom | tr -d ' \n'
}

TMP_JAR="" TMP_ENV=""
cleanup() { rm -f "${TMP_JAR:-}" "${TMP_ENV:-}" 2>/dev/null || true; }
trap cleanup EXIT

# --- env file access ---------------------------------------------------------
env_file_get() { # env_file_get KEY — value from the file, empty if absent
  [[ -f "$ENV_FILE" ]] || return 0
  sed -n "s/^${1}=//p" "$ENV_FILE" | tail -n1
}
cfg() { # cfg KEY [default] — env var, else env file, else default
  local v="${!1:-}"
  [[ -n "$v" ]] || v=$(env_file_get "$1")
  printf '%s' "${v:-${2:-}}"
}
env_set() { # env_set TMPFILE KEY VALUE — replace or append one line
  local tmp="$1" key="$2" val="$3"
  if grep -q "^${key}=" "$tmp" 2>/dev/null; then
    # -F'=' so $1 is the key part (default whitespace FS would never match)
    VAL="$val" awk -F'=' -v k="$key" 'BEGIN{OFS="="} $1==k {print k"="ENVIRON["VAL"]; next} {print}' \
      "$tmp" >"$tmp.tmp" && mv "$tmp.tmp" "$tmp"
  else
    printf '%s=%s\n' "$key" "$val" >>"$tmp"
  fi
}
commit_env_file() { # commit_env_file TMPFILE — move into place only if changed
  local tmp="$1"
  if [[ -f "$ENV_FILE" ]] && diff -q "$ENV_FILE" "$tmp" >/dev/null 2>&1; then
    log "env file already holds the bot key — no changes"
  else
    mkdir -p "$(dirname "$ENV_FILE")"
    chmod 600 "$tmp"
    mv "$tmp" "$ENV_FILE"
    log "updated ${ENV_FILE} (bot key installed, secrets kept local)"
  fi
}

# --- JSON helpers (python3; input via argv so no quoting surprises) -----------
json_field() { # json_field JSON FIELD — top-level field, empty if absent
  python3 -I -c '
import json, sys
try: d = json.loads(sys.argv[1])
except Exception: sys.exit(1)
v = d.get(sys.argv[2]) if isinstance(d, dict) else None
sys.stdout.write("" if v is None else (v if isinstance(v, str) else json.dumps(v)))
' "$1" "$2"
}
json_items() { # json_items JSON — the items array (plain list or {results})
  python3 -I -c '
import json, sys
d = json.loads(sys.argv[1])
if isinstance(d, dict) and isinstance(d.get("results"), list): d = d["results"]
if not isinstance(d, list): sys.exit(1)
sys.stdout.write(json.dumps(d))
' "$1"
}
json_pick_name() { # json_pick_name ITEMS-JSON NAME — item matching name (ci)
  python3 -I -c '
import json, sys
want = sys.argv[2].lower()
for s in json.loads(sys.argv[1]):
    if str(s.get("name", "")).lower() == want:
        sys.stdout.write(json.dumps(s)); break
' "$1" "$2"
}
emails_have() { # emails_have ITEMS-JSON EMAIL — 0 if email present (ci)
  python3 -I -c '
import json, sys
items = json.loads(sys.argv[1]); want = sys.argv[2].lower()
sys.exit(0 if any(str(m.get("email", "")).lower() == want for m in items) else 1)
' "$1" "$2"
}

# --- HTTP layer (sets REPLY_BODY / REPLY_CODE / REPLY_REDIRECT) --------------
REPLY_BODY="" REPLY_CODE="" REPLY_REDIRECT=""
http() { # http METHOD PATH [curl args...]
  local method="$1" path="$2" out bodyfile
  shift 2
  bodyfile=$(mktemp "${TMPDIR:-/tmp}/plane-setup-body.XXXXXX")
  if ! out=$(curl -sS --max-time 60 -X "$method" -o "$bodyfile" \
        -w '%{http_code} %{redirect_url}' "$@" "${PLANE_URL%/}${path}" 2>&1); then
    rm -f "$bodyfile"
    die "curl failed for $method $path: ${out:0:200}"
  fi
  REPLY_CODE="${out%% *}"
  REPLY_REDIRECT="${out#* }"
  REPLY_BODY=$(cat "$bodyfile")
  rm -f "$bodyfile"
  [[ "$REPLY_CODE" =~ ^[0-9]+$ ]] || die "bad HTTP reply for $method $path: ${out:0:200}"
}
V1_KEY=""
v1() { # v1 METHOD PATH [json-body] — v1 external API with the current key
  local method="$1" path="$2" body="${3:-}"
  local -a args=(-H "X-Api-Key: ${V1_KEY}" -H "Content-Type: application/json")
  [[ -n "$body" ]] && args+=(-d "$body")
  http "$method" "/api/v1${path}" "${args[@]}"
}
CSRF=""
sess() { # sess METHOD PATH [curl args...] — session API, cookie jar + CSRF
  local method="$1" path="$2"
  shift 2
  http "$method" "$path" -b "$TMP_JAR" -c "$TMP_JAR" -H "X-CSRFToken: ${CSRF}" "$@"
}
# Session cookie is "session-id" on recent Plane builds, "sessionid" on older
# ones — match both.
has_session() { grep -qsE "session[-_]?id" "$TMP_JAR" 2>/dev/null; }

# --- session bootstrap (first run / key recovery only) ------------------------
refresh_csrf() { # after login the CSRF token lives in the session — re-fetch it
  sess GET "/auth/get-csrf-token/"
  [[ "$REPLY_CODE" == "200" ]] || die "cannot refresh CSRF token after login (HTTP $REPLY_CODE)"
  CSRF=$(json_field "$REPLY_BODY" csrf_token)
  [[ -n "$CSRF" ]] || die "no csrf_token in /auth/get-csrf-token/ response (post-login)"
}

bot_login() { # create a bot session; sign-in, falling back to sign-up
  TMP_JAR=$(mktemp "${TMPDIR:-/tmp}/plane-setup-jar.XXXXXX")
  sess GET "/auth/get-csrf-token/"
  [[ "$REPLY_CODE" == "200" ]] || die "cannot fetch CSRF token (HTTP $REPLY_CODE)"
  CSRF=$(json_field "$REPLY_BODY" csrf_token)
  [[ -n "$CSRF" ]] || die "no csrf_token in /auth/get-csrf-token/ response"

  sess POST "/auth/sign-in/" \
    --data-urlencode "email=${BOT_EMAIL}" --data-urlencode "password=${BOT_PASSWORD}"
  if has_session; then log "bot session: signed in as ${BOT_EMAIL}"; refresh_csrf; return 0; fi

  sess POST "/auth/sign-up/" \
    --data-urlencode "email=${BOT_EMAIL}" --data-urlencode "password=${BOT_PASSWORD}"
  if has_session; then log "bot session: created user ${BOT_EMAIL}"; refresh_csrf; return 0; fi
  if [[ "$REPLY_REDIRECT" == *USER_ALREADY_EXIST* ]]; then
    die "bot user ${BOT_EMAIL} exists but sign-in failed — PLANE_BOT_PASSWORD in ${ENV_FILE} is stale; correct it and re-run"
  fi
  die "bot sign-in/sign-up failed (HTTP $REPLY_CODE, redirect ${REPLY_REDIRECT:0:120})"
}

create_workspace() {
  local body
  body=$(python3 -I -c 'import json, sys; sys.stdout.write(json.dumps({"name": sys.argv[1], "slug": sys.argv[2]}))' \
    "$WS_NAME" "$WS_SLUG")
  sess POST "/api/workspaces/" -H "Content-Type: application/json" -d "$body"
  if [[ "$REPLY_CODE" == "201" ]]; then
    log "workspace '${WS_NAME}' created (slug ${WS_SLUG}, owner ${BOT_EMAIL})"
    return 0
  fi
  if [[ "$REPLY_CODE" == "400" && "$REPLY_BODY" == *[Ss]lug* ]]; then
    die "a workspace with slug '${WS_SLUG}' already exists on this Plane instance and the bot user does not belong to it — resolve it in the Plane UI (delete it or add the bot as admin), then re-run"
  fi
  die "workspace create failed (HTTP $REPLY_CODE): ${REPLY_BODY:0:300}"
}

# Prints slugs of the bot's workspaces, one per line. NOTE: the plain
# GET /api/workspaces/ list endpoint 400s on this Plane build ("The required
# key does not exist."); /api/users/me/workspaces/ is the working one.
session_ws_slugs() {
  local items
  sess GET "/api/users/me/workspaces/"
  [[ "$REPLY_CODE" == "200" ]] || die "list bot workspaces failed (HTTP $REPLY_CODE): $REPLY_BODY"
  items=$(json_items "$REPLY_BODY" 2>/dev/null) || items="[]"
  printf '%s' "$items" | python3 -I -c '
import json, sys
for w in json.load(sys.stdin):
    print(w.get("slug", ""))
'
}

ensure_bot_token() { # needs bot session; prints the bot token value
  # The token value is returned only by the create call (the list serializer
  # hides it), so a labeled token is deleted and re-created to get one.
  local body old
  sess GET "/api/users/api-tokens/"
  [[ "$REPLY_CODE" == "200" ]] || die "list bot API tokens failed (HTTP $REPLY_CODE): $REPLY_BODY"
  old=$(json_items "$REPLY_BODY" | python3 -I -c '
import json, sys
label = sys.argv[1]
for t in json.load(sys.stdin):
    if t.get("label") == label:
        print(t.get("id")); break
' "$BOT_TOKEN_LABEL")
  if [[ -n "$old" ]]; then
    sess DELETE "/api/users/api-tokens/${old}/"
    [[ "$REPLY_CODE" == "200" || "$REPLY_CODE" == "204" ]] \
      || die "delete old bot token failed (HTTP $REPLY_CODE): $REPLY_BODY"
    log "rotated bot API token (old token labeled '${BOT_TOKEN_LABEL}' deleted)"
  fi
  body=$(python3 -I -c 'import json, sys; sys.stdout.write(json.dumps({"label": sys.argv[1]}))' "$BOT_TOKEN_LABEL")
  sess POST "/api/users/api-tokens/" -H "Content-Type: application/json" -d "$body"
  [[ "$REPLY_CODE" == "201" ]] || die "create bot API token failed (HTTP $REPLY_CODE): $REPLY_BODY"
  local tok
  tok=$(json_field "$REPLY_BODY" token)
  [[ -n "$tok" ]] || die "no token value in API token create response"
  printf '%s' "$tok"
}

# --- board objects (v1, as the bot) -------------------------------------------
# Adding a human member headlessly is not possible on this Plane build: the
# invite-accept endpoint (POST /api/workspaces/<slug>/invitations/<pk>/join/)
# requires the invitee's own authenticated session plus the one-time invite
# token (GHSA-4vj8-p63v-8p24 fix), and the token is never exposed by any API.
# So the script creates (or keeps) a pending invite; the human accepts it in
# the Plane UI. No e-mail is sent on this instance — the invite only shows up
# in the Plane UI of the invited account.
ensure_admin_invite() { # $ADMIN_EMAIL is a member, or has a pending admin invite
  local items invite_body
  v1 GET "/workspaces/${WS_SLUG}/members/"
  [[ "$REPLY_CODE" == "200" ]] || die "list workspace members failed (HTTP $REPLY_CODE): $REPLY_BODY"
  items=$(json_items "$REPLY_BODY") || die "unexpected members response: ${REPLY_BODY:0:200}"
  if emails_have "$items" "$ADMIN_EMAIL"; then
    log "${ADMIN_EMAIL} is a workspace member"
    return 0
  fi

  invite_body=$(python3 -I -c 'import json, sys; sys.stdout.write(json.dumps({"email": sys.argv[1], "role": 20}))' \
    "$ADMIN_EMAIL")
  v1 POST "/workspaces/${WS_SLUG}/invitations/" "$invite_body"
  if [[ "$REPLY_CODE" == "201" ]]; then
    log "admin invite created for ${ADMIN_EMAIL} — accept it in the Plane UI to finish"
  elif [[ "$REPLY_CODE" == "400" ]]; then
    # EMAIL_ALREADY_INVITED — a pending invitation exists; nothing to do.
    log "admin invite for ${ADMIN_EMAIL} already pending — accept it in the Plane UI"
  else
    die "create admin invitation failed (HTTP $REPLY_CODE): ${REPLY_BODY:0:300}"
  fi
}

PROJECT_UUID=""
find_or_create_project() {
  local items body
  v1 GET "/workspaces/${WS_SLUG}/projects/"
  [[ "$REPLY_CODE" == "200" ]] || die "list projects failed (HTTP $REPLY_CODE): $REPLY_BODY"
  items=$(json_items "$REPLY_BODY") || die "unexpected projects response: ${REPLY_BODY:0:200}"
  PROJECT_UUID=$(python3 -I -c '
import json, sys
want = sys.argv[1].upper()
for p in json.loads(sys.argv[2]):
    if str(p.get("identifier", "")).upper() == want:
        print(p.get("id")); break
' "$PROJECT_IDENTIFIER" "$items")
  [[ -n "$PROJECT_UUID" ]] && { log "project ${PROJECT_IDENTIFIER} already exists (${PROJECT_UUID})"; return 0; }

  body=$(python3 -I -c '
import json, sys
sys.stdout.write(json.dumps({
    "name": sys.argv[1], "identifier": sys.argv[2], "description": sys.argv[3]}))
' "$PROJECT_NAME" "$PROJECT_IDENTIFIER" \
  "Jarvis work board for the ai-agent-build project — jobs, epics and plans. Maintained by plane-setup; see docs/work-board/PLAN.md.")
  v1 POST "/workspaces/${WS_SLUG}/projects/" "$body"
  [[ "$REPLY_CODE" == "201" ]] || die "create project failed (HTTP $REPLY_CODE): ${REPLY_BODY:0:300}"
  PROJECT_UUID=$(json_field "$REPLY_BODY" id)
  [[ -n "$PROJECT_UUID" ]] || die "no project id in create response: ${REPLY_BODY:0:200}"
  log "project ${PROJECT_NAME} created (identifier ${PROJECT_IDENTIFIER}, ${PROJECT_UUID})"
}

state_ids_in_use() { # prints state ids referenced by work items ("" if unreadable)
  v1 GET "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/work-items/"
  [[ "$REPLY_CODE" == "200" ]] || return 0
  json_items "$REPLY_BODY" 2>/dev/null | python3 -I -c '
import json, sys
items = json.load(sys.stdin)
ids = sorted({str(i.get("state_id")) for i in items if i.get("state_id")})
print(" ".join(ids))
' 2>/dev/null || true
}

converge_states() {
  local items i name group color seq
  v1 GET "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/states/"
  [[ "$REPLY_CODE" == "200" ]] || die "list states failed (HTTP $REPLY_CODE): $REPLY_BODY"
  items=$(json_items "$REPLY_BODY") || die "unexpected states response: ${REPLY_BODY:0:200}"

  for i in "${!DESIRED_STATES[@]}"; do
    IFS='|' read -r name group color <<<"${DESIRED_STATES[$i]}"
    seq=$(( (i + 1) * 1000 ))
    local existing
    existing=$(json_pick_name "$items" "$name")
    if [[ -n "$existing" ]]; then
      local sid sgroup sseq
      sid=$(json_field "$existing" id)
      sgroup=$(json_field "$existing" group)
      sseq=$(json_field "$existing" sequence)
      # The API stores/returns sequence as a float (1000.0) — normalize so the
      # comparison is against the integer we send; without this every run
      # "corrects" (re-PATCHes) all states.
      sseq="${sseq%%.*}"
      if [[ "$sgroup" != "$group" || "$sseq" != "$seq" ]]; then
        local patch
        patch=$(python3 -I -c 'import json, sys; sys.stdout.write(json.dumps({"group": sys.argv[1], "sequence": float(sys.argv[2])}))' \
          "$group" "$seq")
        v1 PATCH "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/states/${sid}/" "$patch"
        [[ "$REPLY_CODE" == "200" ]] || die "patch state '$name' failed (HTTP $REPLY_CODE): $REPLY_BODY"
        log "state '$name' corrected (group $group, sequence $seq)"
      fi
    else
      local body
      body=$(python3 -I -c '
import json, sys
sys.stdout.write(json.dumps({
    "name": sys.argv[1], "color": sys.argv[2], "group": sys.argv[3],
    "sequence": float(sys.argv[4]), "description": ""}))
' "$name" "$color" "$group" "$seq")
      v1 POST "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/states/" "$body"
      # This Plane era returns 200 for state create (older builds 201).
      [[ "$REPLY_CODE" == "200" || "$REPLY_CODE" == "201" ]] \
        || die "create state '$name' failed (HTTP $REPLY_CODE): $REPLY_BODY"
      log "state '$name' created (group $group, sequence $seq)"
    fi
  done

  # Remove the extra default states Plane auto-creates (Todo, In Review,
  # Cancelled, Duplicate, Archived, …) unless a work item references one.
  local used sid sname
  used=$(state_ids_in_use)
  while IFS=$'\t' read -r sid sname; do
    [[ -n "$sid" ]] || continue
    for x in $used; do
      if [[ "$x" == "$sid" ]]; then
        log "keeping default state '$sname' — referenced by a work item"
        sid=""
        break
      fi
    done
    [[ -n "$sid" ]] || continue
    v1 DELETE "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/states/${sid}/"
    [[ "$REPLY_CODE" == "200" || "$REPLY_CODE" == "204" ]] \
      || die "delete default state '$sname' failed (HTTP $REPLY_CODE): $REPLY_BODY"
    log "removed default state '$sname'"
  done < <(python3 -I -c '
import json, sys
items = json.loads(sys.argv[1])
desired = {s.split("|", 1)[0].lower() for s in sys.argv[2:]}
for s in items:
    n = str(s.get("name", ""))
    if n.lower() not in desired:
        sys.stdout.write(str(s.get("id", "")) + "\t" + n + "\n")
' "$items" "${DESIRED_STATES[@]}")
}

ensure_epic_label() {
  local items body
  v1 GET "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/labels/"
  [[ "$REPLY_CODE" == "200" ]] || die "list labels failed (HTTP $REPLY_CODE): $REPLY_BODY"
  items=$(json_items "$REPLY_BODY") || die "unexpected labels response: ${REPLY_BODY:0:200}"
  if python3 -I -c '
import json, sys
sys.exit(0 if any(str(l.get("name", "")).lower() == "epic" for l in json.loads(sys.argv[1])) else 1)
' "$items"; then
    return 0
  fi
  body=$(python3 -I -c '
import json, sys
sys.stdout.write(json.dumps({
    "name": "epic", "color": sys.argv[1], "description": sys.argv[2]}))
' "$EPIC_LABEL_COLOR" \
  "Epic — a theme of work spanning multiple jobs (see docs/work-board/PLAN.md).")
  v1 POST "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/labels/" "$body"
  [[ "$REPLY_CODE" == "201" ]] || die "create epic label failed (HTTP $REPLY_CODE): ${REPLY_BODY:0:300}"
  log "label 'epic' created"
}

# --- final verification --------------------------------------------------------
# verify sets PENDING_ADMIN=1 when $ADMIN_EMAIL still has to accept the invite
# in the Plane UI (not an error — the API cannot do it headlessly).
PENDING_ADMIN=0
verify() {
  local items ok=0
  PENDING_ADMIN=0
  log "verifying final state…"
  v1 GET "/users/me/"
  [[ "$REPLY_CODE" == "200" && "$(json_field "$REPLY_BODY" email)" == "$BOT_EMAIL" ]] \
    || { log "  ✗ API key owner is not the bot"; ok=1; }

  v1 GET "/workspaces/${WS_SLUG}/members/"
  [[ "$REPLY_CODE" == "200" ]] || { log "  ✗ cannot list members"; ok=1; }
  items=$(json_items "$REPLY_BODY" 2>/dev/null || echo "[]")
  emails_have "$items" "$BOT_EMAIL" || { log "  ✗ bot not a workspace member"; ok=1; }
  emails_have "$items" "$ADMIN_EMAIL" || PENDING_ADMIN=1

  v1 GET "/workspaces/${WS_SLUG}/projects/"
  [[ "$REPLY_CODE" == "200" ]] || { log "  ✗ cannot list projects"; ok=1; }
  json_items "$REPLY_BODY" 2>/dev/null | python3 -I -c '
import json, sys
want = sys.argv[1].upper()
sys.exit(0 if any(str(p.get("identifier", "")).upper() == want for p in json.load(sys.stdin)) else 1)
' "$PROJECT_IDENTIFIER" || { log "  ✗ project ${PROJECT_IDENTIFIER} missing"; ok=1; }

  v1 GET "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/states/"
  [[ "$REPLY_CODE" == "200" ]] || { log "  ✗ cannot list states"; ok=1; }
  items=$(json_items "$REPLY_BODY" 2>/dev/null || echo "[]")
  local missing=""
  for s in "${DESIRED_STATES[@]}"; do
    [[ -n "$(json_pick_name "$items" "${s%%|*}")" ]] || missing+=" ${s%%|*}"
  done
  [[ -z "$missing" ]] || { log "  ✗ states missing:$missing"; ok=1; }

  v1 GET "/workspaces/${WS_SLUG}/projects/${PROJECT_UUID}/labels/"
  [[ "$REPLY_CODE" == "200" ]] || { log "  ✗ cannot list labels"; ok=1; }
  json_items "$REPLY_BODY" 2>/dev/null | python3 -I -c '
import json, sys
sys.exit(0 if any(str(l.get("name", "")).lower() == "epic" for l in json.load(sys.stdin)) else 1)
' || { log "  ✗ epic label missing"; ok=1; }

  if [[ "$ok" == "0" ]]; then
    log "  ✓ workspace ${WS_SLUG}, project ${PROJECT_IDENTIFIER}, 8 states, epic label, member ${BOT_EMAIL}"
    if [[ "$PENDING_ADMIN" == "1" ]]; then
      log "  ⚠ ${ADMIN_EMAIL} not a member yet — pending invite, accept it in the Plane UI"
    else
      log "  ✓ members: ${BOT_EMAIL} + ${ADMIN_EMAIL}"
    fi
  fi
  return "$ok"
}

# --- main ----------------------------------------------------------------------
main() {
  PLANE_URL=$(cfg PLANE_URL)
  [[ -n "$PLANE_URL" ]] || die "PLANE_URL not set (env file: ${ENV_FILE})"
  V1_KEY=$(cfg PLANE_API_KEY)
  [[ -n "$V1_KEY" ]] || die "PLANE_API_KEY not set (env file: ${ENV_FILE})"
  BOT_EMAIL=$(cfg PLANE_BOT_EMAIL "$DEFAULT_BOT_EMAIL")
  ADMIN_EMAIL=$(cfg PLANE_ADMIN_EMAIL "$DEFAULT_ADMIN_EMAIL")
  BOT_PASSWORD=$(cfg PLANE_BOT_PASSWORD)
  local admin_key; admin_key=$(cfg PLANE_ADMIN_API_KEY)

  log "plane url: ${PLANE_URL}"
  log "api key:   $(mask "$V1_KEY")"
  log "bot user:  ${BOT_EMAIL}"

  v1 GET "/users/me/"
  [[ "$REPLY_CODE" == "200" ]] || die "PLANE_API_KEY rejected by ${PLANE_URL} (HTTP $REPLY_CODE)"
  local me_email; me_email=$(json_field "$REPLY_BODY" email)
  log "key owner: ${me_email}"

  local env_dirty=0 new_bot_key="" new_admin_key="" new_admin_email="" slugs

  if [[ "$me_email" == "$BOT_EMAIL" ]]; then
    # --- the env key is the bot's ---------------------------------------------
    v1 GET "/workspaces/${WS_SLUG}/projects/"
    if [[ "$REPLY_CODE" == "200" ]]; then
      log "workspace ${WS_SLUG} present — steady state"
    else
      # The bot key is valid but cannot see the workspace (deleted, or the bot
      # was removed). Note: v1 answers 403 for both "not a member" and "does
      # not exist", so the authoritative check is the bot's own session list.
      [[ -n "$BOT_PASSWORD" ]] \
        || die "the bot cannot see workspace ${WS_SLUG} and ${ENV_FILE} has no PLANE_BOT_PASSWORD for session recovery"
      bot_login
      slugs=$(session_ws_slugs)
      if grep -qx "$WS_SLUG" <<<"$slugs"; then
        v1 GET "/workspaces/${WS_SLUG}/projects/"
        die "the bot belongs to workspace ${WS_SLUG} but its v1 API still rejects it (HTTP ${REPLY_CODE}) — check the bot's membership in the Plane UI"
      fi
      log "workspace ${WS_SLUG} missing — (re)creating it"
      create_workspace
      log "workspace restored; env key unchanged"
    fi
  else
    # --- the env key is not the bot's: first run or key recovery --------------
    [[ -n "$BOT_PASSWORD" ]] || { BOT_PASSWORD=$(gen_password); log "generated bot password (kept only in ${ENV_FILE})"; }
    bot_login
    slugs=$(session_ws_slugs)
    if grep -qx "$WS_SLUG" <<<"$slugs"; then
      log "workspace ${WS_SLUG} already owned by the bot — no workspace changes"
    else
      create_workspace
    fi
    # The token value is only ever returned by the create call, so the labeled
    # token is (re)created and the env file gets the fresh value.
    local old_key="$V1_KEY"
    new_bot_key=$(ensure_bot_token)
    V1_KEY="$new_bot_key"
    env_dirty=1
    log "v1 api key switched to the bot token ($(mask "$V1_KEY"))"
    if [[ -z "$admin_key" ]]; then
      # The pre-existing (human) key — preserve it as the admin backup key.
      new_admin_key="$old_key"
      [[ -n "$(cfg PLANE_ADMIN_EMAIL)" ]] || new_admin_email="$me_email"
    fi
  fi

  # --- board objects (all v1, as the bot) ------------------------------------
  ensure_admin_invite
  find_or_create_project
  converge_states
  ensure_epic_label

  # --- env file ----------------------------------------------------------------
  if [[ "$env_dirty" == "1" ]]; then
    TMP_ENV=$(mktemp "${TMPDIR:-/tmp}/plane-setup-env.XXXXXX")
    if [[ -f "$ENV_FILE" ]]; then cp "$ENV_FILE" "$TMP_ENV"; else : >"$TMP_ENV"; fi
    env_set "$TMP_ENV" PLANE_API_KEY "$new_bot_key"
    env_set "$TMP_ENV" PLANE_BOT_EMAIL "$BOT_EMAIL"
    env_set "$TMP_ENV" PLANE_BOT_PASSWORD "$BOT_PASSWORD"
    if [[ -n "$new_admin_key" ]]; then env_set "$TMP_ENV" PLANE_ADMIN_API_KEY "$new_admin_key"; fi
    if [[ -n "$new_admin_email" ]]; then env_set "$TMP_ENV" PLANE_ADMIN_EMAIL "$new_admin_email"; fi
    commit_env_file "$TMP_ENV"
    TMP_ENV=""
  else
    log "env file already holds the bot key — no changes"
  fi

  verify || exit 1
  if [[ "$PENDING_ADMIN" == "1" ]]; then
    log "converged — board is in the planned state; ${ADMIN_EMAIL} still has a pending invite (accept it in the Plane UI to finish)"
  else
    log "converged — board is in the planned state"
  fi
}

main "$@"
