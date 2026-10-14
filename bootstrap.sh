#!/usr/bin/env bash
#
# bootstrap.sh
#
# Run once on a fresh Ubuntu system with internet access. NVIDIA drivers
# may NOT be installed yet -- that's fine, Claude handles it from here.
#
# This script does almost nothing itself. Its only job is to:
#   1. Install prerequisites needed to run Claude Code at all (curl, git, node),
#      and create a dedicated 'agent' user with passwordless sudo that the
#      agent runs as. Everything below lives in that user's home.
#   2. Install Claude Code
#   3. Give the agent its own GitHub identity (a GitHub App; short-lived
#      tokens are minted on demand from the app's private key)
#   4. Hand Claude the task list (TASKS.md) and shell access, with
#      instructions to build the whole system itself -- including
#      installing NVIDIA drivers, installing Ollama, standing up RabbitMQ,
#      writing the Go listener, and delegating bulk/low-stakes coding work
#      to the local Ollama model once it exists, to conserve API usage.
#   5. Build jarvis-discord (cmd/jarvis-discord in the repo) so the agent
#      can notify you and ask you questions over Discord DMs while it works.
#      See docs/discord.md for creating the bot.
#   6. Run the agent in two stages via agent-run: driver/CUDA on a cheap
#      model (haiku), escalating to sonnet if that fails, then the rest on
#      sonnet.
#   7. Install agent-reboot and agent-resume.service so the agent can
#      reboot (e.g. after the NVIDIA driver install) and pick up its
#      conversation where it left off once the box is back up.
#
# Usage:
#   export ANTHROPIC_API_KEY=...
#   export GITHUB_APP_ID=...                 # from the app's settings page
#   export GITHUB_APP_KEY=/path/to/app.pem   # copy it onto this box first
#   export REPO_URL=https://github.com/<org>/<repo>.git
#   export DISCORD_BOT_TOKEN=...             # docs/discord.md
#   export DISCORD_OWNER_ID=...              # your Discord user ID
#   chmod +x bootstrap.sh
#   ./bootstrap.sh
#
# Or put those lines (without "export") in an env file, e.g. on network
# storage next to the .pem, and pass its path. See bootstrap.env.example.
# GITHUB_APP_KEY may then be relative to the env file's folder. Values are
# copied onto this box, so the share isn't needed after bootstrap.
#   bash /mnt/setup/setup/bootstrap.sh /mnt/setup/setup/bootstrap.env
#
# Optional:
#   GITHUB_ORG        org the app is installed on and the agent may build
#                     new repos in (default: the org in REPO_URL)
#   GIT_AUTHOR_NAME   (default: ai-agent)
#   GIT_AUTHOR_EMAIL  (default: ai-agent@users.noreply.github.com)
#   AGENT_USER        user the agent runs as (default: agent)
#   PROJECT_DIR       (default: /home/$AGENT_USER/ai-agent-build)
#   GO_VERSION        (default: 1.26.2)
#   AGENT_STAGE1_MODEL  model for the NVIDIA driver/CUDA stage (default: haiku).
#                       If the GPU isn't working afterwards it escalates to the
#                       stage-2 model. Set to sonnet to skip the cheap pass.
#   AGENT_STAGE2_MODEL  model for everything after that (default: sonnet)
#   NAS_SHARE         SMB share to mount read-write at /mnt/jarvis for the
#                     agent, e.g. //192.168.1.20/JarvissFolder. Needs
#                     NAS_USER and NAS_PASSWORD (a NAS account limited to
#                     that share). The password is kept root-only on the box.
#
# Run this as your own (sudo-capable) user, not as root. It only uses sudo
# to set up the system and the agent user; the agent never runs as you.
#
# The repo must already contain TASKS.md on its default branch.
# Never commit the .pem file.

set -euo pipefail

if [[ -n "${1:-}" ]]; then
  [[ -r "$1" ]] || { echo "ERROR: can't read env file $1" >&2; exit 1; }
  set -a
  # shellcheck disable=SC1090
  source "$1"
  set +a
  if [[ -n "${GITHUB_APP_KEY:-}" && "$GITHUB_APP_KEY" != /* ]]; then
    GITHUB_APP_KEY="$(cd "$(dirname "$1")" && pwd)/$GITHUB_APP_KEY"
  fi
fi

REPO_URL="${REPO_URL:-}"
AGENT_USER="${AGENT_USER:-agent}"
AGENT_HOME="/home/$AGENT_USER"
PROJECT_DIR="${PROJECT_DIR:-$AGENT_HOME/ai-agent-build}"
GITHUB_APP_ID="${GITHUB_APP_ID:-}"
GITHUB_APP_KEY="${GITHUB_APP_KEY:-}"
GITHUB_ORG="${GITHUB_ORG:-$(printf '%s' "$REPO_URL" | cut -d/ -f4)}"
GIT_AUTHOR_NAME="${GIT_AUTHOR_NAME:-ai-agent}"
GIT_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-ai-agent@users.noreply.github.com}"
GO_VERSION="${GO_VERSION:-1.26.2}"
AGENT_STAGE1_MODEL="${AGENT_STAGE1_MODEL:-haiku}"
AGENT_STAGE2_MODEL="${AGENT_STAGE2_MODEL:-sonnet}"

log() { printf '\n==> %s\n' "$1"; }
die() { printf '\nERROR: %s\n' "$1" >&2; exit 1; }
# Run a command as the agent user, from its home (it can't read yours).
as_agent() { sudo -u "$AGENT_USER" -H env -C "$AGENT_HOME" "$@"; }
# Write stdin to a file only the agent user can read.
agent_write() { as_agent bash -c 'umask 077; cat > "$1"' _ "$1"; }

[[ $EUID -eq 0 ]] && die "Run as your normal user (with sudo rights), not as root."

[[ -z "$REPO_URL" ]] && die "Set REPO_URL (https://github.com/<org>/<repo>.git)."
[[ "$REPO_URL" == https://github.com/* ]] || die "REPO_URL must be an https://github.com/ URL."
[[ -z "$GITHUB_APP_ID" ]] && die "Set GITHUB_APP_ID."
[[ -f "$GITHUB_APP_KEY" ]] || die "Set GITHUB_APP_KEY to the path of the app's .pem file."
GITHUB_APP_KEY=$(realpath "$GITHUB_APP_KEY")
[[ -z "${ANTHROPIC_API_KEY:-}" ]] && die "Set ANTHROPIC_API_KEY first."
[[ -z "${DISCORD_BOT_TOKEN:-}" ]] && die "Set DISCORD_BOT_TOKEN (see docs/discord.md)."
[[ -z "${DISCORD_OWNER_ID:-}" ]] && die "Set DISCORD_OWNER_ID (your Discord user ID)."
if [[ -n "${NAS_SHARE:-}" ]]; then
  [[ "$NAS_SHARE" == //* ]] || die "NAS_SHARE must look like //host/share."
  [[ -n "${NAS_USER:-}" && -n "${NAS_PASSWORD:-}" ]] || die "NAS_SHARE is set; also set NAS_USER and NAS_PASSWORD."
fi

log "Installing prerequisites"
sudo apt-get update
sudo apt-get install -y curl git jq openssl build-essential ca-certificates
sudo apt-get install -y gh || log "gh not available; agent will push branches only"

if ! command -v node &>/dev/null; then
  curl -fsSL https://deb.nodesource.com/setup_lts.x | sudo -E bash -
  sudo apt-get install -y nodejs
fi

log "Creating the '$AGENT_USER' user"
if ! id "$AGENT_USER" &>/dev/null; then
  sudo useradd --create-home --shell /bin/bash --comment "AI build agent" "$AGENT_USER"
fi
# The agent works unattended (driver installs, apt, docker, reboots), so it
# gets passwordless sudo. Your own account is untouched.
SUDOERS=$(mktemp)
echo "$AGENT_USER ALL=(ALL) NOPASSWD: ALL" > "$SUDOERS"
sudo visudo -cf "$SUDOERS" >/dev/null || die "Generated sudoers rule is invalid."
sudo install -m 440 "$SUDOERS" "/etc/sudoers.d/$AGENT_USER"
rm "$SUDOERS"
as_agent sudo -n true || die "Passwordless sudo for '$AGENT_USER' is not working."

if [[ -n "${NAS_SHARE:-}" ]]; then
  log "Mounting $NAS_SHARE at /mnt/jarvis"
  sudo apt-get install -y cifs-utils
  NAS_CRED=/etc/jarvis-nas.credentials
  printf 'username=%s\npassword=%s\n' "$NAS_USER" "$NAS_PASSWORD" |
    sudo sh -c 'umask 077; cat > "$1"' _ "$NAS_CRED"
  sudo mkdir -p /mnt/jarvis
  # fstab separates fields with spaces, so escape any in the share name.
  FSTAB_LINE="$(printf '%s' "$NAS_SHARE" | sed 's/ /\\040/g') /mnt/jarvis cifs credentials=$NAS_CRED,uid=$AGENT_USER,gid=$AGENT_USER,file_mode=0660,dir_mode=0770,vers=3.0,_netdev,nofail 0 0"
  { grep -v ' /mnt/jarvis cifs ' /etc/fstab || true; echo "$FSTAB_LINE"; } > /tmp/fstab.jarvis
  sudo install -m 644 /tmp/fstab.jarvis /etc/fstab
  rm /tmp/fstab.jarvis
  sudo systemctl daemon-reload
  mountpoint -q /mnt/jarvis || sudo mount /mnt/jarvis
  as_agent test -w /mnt/jarvis || die "/mnt/jarvis is mounted but $AGENT_USER can't write to it. Check the NAS user's permissions."
fi

log "Installing Go $GO_VERSION"
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go$GO_VERSION "; then
  GO_TGZ="go$GO_VERSION.linux-amd64.tar.gz"
  GO_SHA=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
    jq -r --arg f "$GO_TGZ" '.[].files[] | select(.filename == $f) | .sha256')
  [[ -n "$GO_SHA" ]] || die "Go $GO_VERSION not found on go.dev."
  curl -fsSL "https://go.dev/dl/$GO_TGZ" -o "/tmp/$GO_TGZ"
  echo "$GO_SHA  /tmp/$GO_TGZ" | sha256sum -c - || die "Go download checksum mismatch."
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "/tmp/$GO_TGZ"
  rm "/tmp/$GO_TGZ"
fi
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' | sudo tee /etc/profile.d/go.sh >/dev/null
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
go version

log "Installing Claude Code"
sudo npm install -g @anthropic-ai/claude-code
claude --version || die "Claude Code install failed."

log "Setting up GitHub App authentication"
CONF="$AGENT_HOME/.config/ghapp"
as_agent mkdir -p -m 700 "$AGENT_HOME/.config" "$CONF"
sudo install -o "$AGENT_USER" -g "$AGENT_USER" -m 600 "$GITHUB_APP_KEY" "$CONF/key.pem"
printf 'GHAPP_ID=%q\nGHAPP_ORG=%q\n' "$GITHUB_APP_ID" "$GITHUB_ORG" | agent_write "$CONF/env"

# ghapp-token: prints a fresh installation token (valid ~1 hour, cached).
sudo tee /usr/local/bin/ghapp-token >/dev/null <<'GHAPP_EOF'
#!/usr/bin/env bash
set -euo pipefail
CONF="${GHAPP_CONF:-$HOME/.config/ghapp}"
# shellcheck disable=SC1091
source "$CONF/env"
CACHE="$CONF/token.cache"
now=$(date +%s)

if [[ -f "$CACHE" ]]; then
  read -r exp tok < "$CACHE" || true
  if [[ -n "${tok:-}" && $(( ${exp:-0} - now )) -gt 300 ]]; then
    echo "$tok"
    exit 0
  fi
fi

b64() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
hdr=$(printf '{"alg":"RS256","typ":"JWT"}' | b64)
pay=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "$GHAPP_ID" | b64)
sig=$(printf '%s.%s' "$hdr" "$pay" | openssl dgst -sha256 -sign "$CONF/key.pem" | b64)
jwt="$hdr.$pay.$sig"

api() {
  curl -fsS -H "Authorization: Bearer $jwt" \
       -H "Accept: application/vnd.github+json" "$@"
}

inst=$(api "https://api.github.com/orgs/$GHAPP_ORG/installation" | jq -r .id)
resp=$(api -X POST "https://api.github.com/app/installations/$inst/access_tokens")
tok=$(jq -r .token <<<"$resp")
exp=$(date -d "$(jq -r .expires_at <<<"$resp")" +%s)

umask 077
echo "$exp $tok" > "$CACHE"
echo "$tok"
GHAPP_EOF
sudo chmod 755 /usr/local/bin/ghapp-token

as_agent ghapp-token >/dev/null || die "Could not get a GitHub App token. Check GITHUB_APP_ID, the .pem, and that the app is installed on the org."

as_agent git config --global user.name "$GIT_AUTHOR_NAME"
as_agent git config --global user.email "$GIT_AUTHOR_EMAIL"
as_agent git config --global credential.helper \
  '!f() { [ "$1" = get ] && { echo username=x-access-token; echo "password=$(ghapp-token)"; }; }; f'

log "Cloning project repo"
if as_agent test -d "$PROJECT_DIR/.git"; then
  as_agent git -C "$PROJECT_DIR" pull
else
  as_agent git clone "$REPO_URL" "$PROJECT_DIR"
fi
as_agent test -f "$PROJECT_DIR/TASKS.md" || die "TASKS.md not found in repo."

log "Setting up Discord notify/ask"
JCONF="$AGENT_HOME/.config/jarvis"
as_agent mkdir -p -m 700 "$JCONF"
printf 'DISCORD_BOT_TOKEN=%s\nDISCORD_OWNER_ID=%s\n' \
  "$DISCORD_BOT_TOKEN" "$DISCORD_OWNER_ID" | agent_write "$JCONF/discord.env"
as_agent bash -c 'cd "$1" && /usr/local/go/bin/go build -o "$2" ./cmd/jarvis-discord' \
  _ "$PROJECT_DIR" "$AGENT_HOME/jarvis-discord"
sudo install -m 755 "$AGENT_HOME/jarvis-discord" /usr/local/bin/jarvis-discord
as_agent rm "$AGENT_HOME/jarvis-discord"
as_agent jarvis-discord notify "Bootstrap started on $(hostname). Handing over to Claude Code." ||
  die "Could not DM you on Discord. Check the token, your user ID, and that the bot shares a server with you."

log "Setting up resume-after-reboot"
sudo install -m 755 "$PROJECT_DIR/scripts/agent-reboot" "$PROJECT_DIR/scripts/agent-resume" "$PROJECT_DIR/scripts/agent-run" /usr/local/bin/

# jarvis-discord ask can block for hours waiting on me; lift the Bash
# tool's 10-minute cap so those calls aren't killed.
export BASH_MAX_TIMEOUT_MS=86400000

# After a reboot nothing from this shell survives, so save what the
# resumed run needs.
AGENT_PATH="/usr/local/go/bin:$AGENT_HOME/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
printf 'ANTHROPIC_API_KEY=%s\nPATH=%s\nBASH_MAX_TIMEOUT_MS=%s\nAGENT_STAGE1_MODEL=%s\nAGENT_STAGE2_MODEL=%s\n' \
  "$ANTHROPIC_API_KEY" "$AGENT_PATH" "$BASH_MAX_TIMEOUT_MS" \
  "$AGENT_STAGE1_MODEL" "$AGENT_STAGE2_MODEL" | agent_write "$JCONF/agent.env"
as_agent rm -f "$JCONF/resume-pending" "$JCONF/resume-count" "$JCONF/stage"
as_agent mkdir -p "$AGENT_HOME/.local/state/jarvis"

sudo tee /etc/systemd/system/agent-resume.service >/dev/null <<UNIT_EOF
[Unit]
Description=Resume the bootstrap agent after a reboot
Wants=network-online.target
After=network-online.target
ConditionPathExists=$JCONF/resume-pending

[Service]
Type=simple
User=$AGENT_USER
WorkingDirectory=$PROJECT_DIR
EnvironmentFile=$JCONF/agent.env
ExecStart=/usr/local/bin/agent-resume
StandardOutput=append:$AGENT_HOME/.local/state/jarvis/agent.log
StandardError=append:$AGENT_HOME/.local/state/jarvis/agent.log

[Install]
WantedBy=multi-user.target
UNIT_EOF
sudo systemctl daemon-reload
sudo systemctl enable agent-resume.service

# From here on, tell me on Discord if the script dies.
trap 'as_agent jarvis-discord notify "bootstrap.sh failed on $(hostname) at line $LINENO (exit $?)." || true' ERR

log "Writing agent briefs"
# read (not $(... <<EOF)) so every bash version parses this the same way.
read -r -d '' COMMON <<'COMMON_EOF' || true
You are running as the '@AGENT_USER@' user, which has passwordless sudo.
Use sudo for system changes rather than asking me for a password.

Talking to me: this run is non-interactive and I am not watching the
terminal, so never end the run just to ask me something. Instead:
- jarvis-discord ask "question"  DMs me and blocks until I answer, then
  prints my answer. Add -choices "a,b,c" to give me buttons (I can still
  type a free-form reply). Run it with the Bash tool's longest timeout.
  Use it before any decision only I can make, before anything risky or
  hard to undo, and when you are stuck.
- jarvis-discord notify "message"  DMs me without waiting. Use it when
  you finish a phase, open a PR (include the link), hit a failure you
  are working around, or are about to reboot.
Keep messages short and lead with what you need from me.

Rebooting: when a reboot is needed (e.g. after the NVIDIA driver
install), commit and push any work in progress, then run
agent-reboot "short reason". It asks me on Discord and reboots if I
approve; you will then be resumed automatically in this same
conversation. If it exits 2, I said no: read my reply and carry on.
Never reboot any other way.

Git rules: this repo is your own, and you may create additional repos
under the GitHub org '@GITHUB_ORG@' for separate components when it
makes sense (if the app lacks permission to create repos, ask me).
Do not touch anything outside that org. In every repo, never commit
or push directly to main. For each phase, work on a branch named
phase-<n>-<short-slug>, commit in small logical commits with clear
messages, push the branch, and open a pull request. Git pushes
authenticate automatically. For gh, run it as
GH_TOKEN=$(ghapp-token) gh ... because the token expires hourly.
I review and merge. Never commit secrets: no API keys, tokens, .pem
files, or .env files; add them to .gitignore first.
COMMON_EOF
COMMON=${COMMON//@GITHUB_ORG@/$GITHUB_ORG}
COMMON=${COMMON//@AGENT_USER@/$AGENT_USER}
if [[ -n "${NAS_SHARE:-}" ]]; then
  COMMON+="

Network storage: my NAS share is mounted read-write at /mnt/jarvis (it
comes back on its own after a reboot). Use it for things worth keeping
off this box. setup/ in it is read-only and holds secrets: never copy
from it into a repo, and don't try to change it."
fi

{
cat <<'STAGE1_EOF'
You are bootstrapping a local AI agent build on a fresh Ubuntu machine
(RTX 4090). This is STAGE 1 of 2, and it is only this: get the NVIDIA
driver and CUDA working. NVIDIA drivers are NOT installed yet.

Install the driver with `ubuntu-drivers autoinstall` (not .run
installers), install the CUDA toolkit, reboot via agent-reboot when
required, and finish by confirming nvidia-smi shows the GPU and nvcc
works. Then message me with jarvis-discord notify and stop. Do not start
on any other part of TASKS.md.

If you hit a problem you cannot resolve in two attempts (driver won't
load, Secure Boot / MOK trouble, nouveau conflicts, version mismatches,
anything strange), stop and say so plainly rather than trying
workarounds: a stronger model will take over this same conversation.

STAGE1_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage1.txt"

{
cat <<'STAGE2_EOF'
You are continuing a local AI agent build on an Ubuntu machine. STAGE 1
(NVIDIA driver + CUDA) is done and nvidia-smi works; confirm that quickly
and move on. Read TASKS.md for the full task list. Install Ollama and
pull the coding model, stand up RabbitMQ, build the Go listener, then
continue through voice integration and sandboxing. Once Ollama is up,
delegate bulk/low-stakes coding to it to conserve my API usage.

STAGE2_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage2.txt"

# Run the staged agent as the agent user, with the same environment the
# resume service gives it after a reboot.
agent_status=0
as_agent bash -c 'set -a; source "$1"; set +a; cd "$2" && exec agent-run' \
  _ "$JCONF/agent.env" "$PROJECT_DIR" || agent_status=$?
as_agent jarvis-discord notify "Claude Code finished its bootstrap run on $(hostname) (exit $agent_status)." || true
exit "$agent_status"
