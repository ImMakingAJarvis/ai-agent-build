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
#   6. Run the agent in stages via agent-run: driver/CUDA on a cheap
#      model (haiku), escalating to sonnet if that fails; then sonnet sets
#      up a local "brain" model on Ollama that can drive Claude Code
#      itself (agent-local) and stops. You get a Discord checkpoint to look
#      it over, and from then on the local model runs the build (pulling
#      the coding model, then the queue system) in rounds you approve on
#      Discord, with sonnet on call when it gets stuck.
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
as_agent bash -c 'cd "$1" && /usr/local/go/bin/go build -o "$2" ./cmd/jarvis-jobs' \
  _ "$PROJECT_DIR" "$AGENT_HOME/jarvis-jobs"
sudo install -m 755 "$AGENT_HOME/jarvis-jobs" /usr/local/bin/jarvis-jobs
as_agent rm "$AGENT_HOME/jarvis-jobs"
as_agent jarvis-discord notify "Bootstrap started on $(hostname). Handing over to Claude Code." ||
  die "Could not DM you on Discord. Check the token, your user ID, and that the bot shares a server with you."

log "Setting up resume-after-reboot"
sudo install -m 755 "$PROJECT_DIR/scripts/agent-reboot" "$PROJECT_DIR/scripts/agent-resume" "$PROJECT_DIR/scripts/agent-run" "$PROJECT_DIR/scripts/agent-local" /usr/local/bin/

# jarvis-discord ask can block for hours waiting on me; lift the Bash
# tool's 10-minute cap so those calls aren't killed.
export BASH_MAX_TIMEOUT_MS=86400000

# After a reboot nothing from this shell survives, so save what the
# resumed run needs.
AGENT_PATH="/usr/local/go/bin:$AGENT_HOME/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
printf 'ANTHROPIC_API_KEY=%s\nPATH=%s\nBASH_MAX_TIMEOUT_MS=%s\nAGENT_STAGE1_MODEL=%s\nAGENT_STAGE2_MODEL=%s\n' \
  "$ANTHROPIC_API_KEY" "$AGENT_PATH" "$BASH_MAX_TIMEOUT_MS" \
  "$AGENT_STAGE1_MODEL" "$AGENT_STAGE2_MODEL" | agent_write "$JCONF/agent.env"
as_agent rm -f "$JCONF/resume-pending" "$JCONF/resume-count" "$JCONF/stage" "$JCONF/driver"
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
You are continuing a local AI agent build on an Ubuntu machine (RTX 4090
24GB VRAM, 128GB RAM). STAGE 1 (NVIDIA driver + CUDA) is done and
nvidia-smi works; confirm that quickly and move on. TASKS.md is the full
task list, for context.

This is STAGE 2, and it is only this: get a local model running that can
take over your job. From stage 3 on, a local model (the "brain") runs
this same Claude Code harness through Ollama's Anthropic-compatible API,
using the same tools, jarvis-discord and agent-reboot, and drives the
rest of the build, delegating code-writing to a coding model. Your job
is to set that up and prove it works, so the API is only needed when the
local model gets stuck.

1. Install Ollama as a systemd service that survives reboots, via
   scripts/install_ollama.sh as described in TASKS.md Phase 2 (verify it
   listens on localhost:11434). It must be recent enough to serve the
   Anthropic Messages API (/v1/messages; v0.14.0 or later).
2. Choose the brain model. It must: call tools reliably through Claude
   Code (that is what makes decisions possible), run with a context
   window of at least 64K tokens (Claude Code's own prompt is large), and
   fit fully in VRAM at that context, KV cache included. The light model
   in TASKS.md (qwen3.6:27b) is the first candidate; check what is
   actually in the Ollama library today.
3. Decide whether the brain can also be the coding model, or whether a
   separate coding model (TASKS.md suggests qwen3-coder-next, offloaded to
   system RAM) is worth it. Recommend one; do not pull a separate coding
   model, the brain does that in stage 3 after I agree.
4. Write a Modelfile for the brain in the repo (models/) that sets the
   context length, create it in Ollama, and keep it loaded in VRAM
   (keep-alive forever for now; the idle unloader comes later in Phase 3).
5. Write ~/.config/jarvis/local.env with exactly these lines:
     LOCAL_MODEL=<the Ollama name of the brain>
     LOCAL_CODER_MODEL=<coding model's Ollama name, or empty for same>
6. Run `agent-local check` until it passes. It runs a Claude Code session
   on the brain and makes it write a file with the Bash tool. If it
   fails, look at why (context too small, tool-call format, model too
   weak) and fix it; try another model if you must. If you cannot get it
   to pass after a few honest attempts, ask me on Discord.
7. Write docs/local-brain.md for me: what you installed and where, which
   models and why (including the coding-model recommendation), context
   length, VRAM use (ollama ps, nvidia-smi), how to run agent-local
   myself, and how to undo it all. Commit everything on branch
   phase-2-local-brain and open a PR.

Then stop. Do not start on anything else. agent-run checks the result
and messages me; I decide when the local model takes over.

STAGE2_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage2.txt"

{
cat <<'STAGE3L_EOF'
You are the local model running this AI agent build on an Ubuntu machine
(RTX 4090, 128GB RAM). Sonnet set you up and handed the build over to
you; I approved it. You make the decisions now.

Work now comes from the queue. This round you do ONE job, given below
after the "YOUR JOB THIS ROUND" line, and then stop. Do not pick other
work, and do not read TASKS.md (it is history and far too big for your
context window). If no job text follows (an older loop
started this round), run `jarvis-jobs next`, read it with
`jarvis-jobs show <id>`, do that job, and record the result at the end
with `jarvis-jobs finish <id> <0 if done, 1 if not>`. Docs: docs/local-brain.md. Run
`GH_TOKEN=$(ghapp-token) gh pr list --state all` to see what is already
merged or waiting for review. If the job's work is already on main, say
so with evidence in the job's PR or Discord note instead of redoing it.

Keep your context small, it is 64K tokens: read files in parts (offset
and limit), grep instead of cat for big files, pipe long command output
through head or tail, and never print a whole log.

How to work:
- Plan first, then build in small steps. If a "coder" subagent is
  available, hand it each well-specified coding task (files, goal, how to
  test) and review what it did; do the planning, reviewing, testing and
  committing yourself.
- Build and test as you go (go build ./..., go test ./...). Do not open a
  PR for code that does not build.
- A job is done only when every acceptance criterion in it passes. The PR
  says how each was checked and what was skipped.
- Finish the round with a pushed branch, an open PR, and a jarvis-discord
  notify with the PR link. Then end your run; the loop records the result
  and I pick what happens next.
- If you are stuck on the same problem after three tries, say so on
  Discord with what you tried and end the run. I can hand it to Sonnet.

STAGE3L_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage3-local.txt"

{
cat <<'STAGE3I_EOF'
You are the SMALL model running this AI agent build: the 3B helper on
the Ryzen iGPU (port 11435), not the 27B brain on the 4090. I picked
you for this round usually because the job needs the 4090 free (for
example the MoE VRAM sweeps) or to keep the round off the paid API.
The brain may be stopped while you work — that is expected, do not try
to restart it.

Work now comes from the queue. This round you do ONE job, given below
after the "YOUR JOB THIS ROUND" line, and then stop. If no job text
follows, run `jarvis-jobs next`, read it with `jarvis-jobs show <id>`,
do that job, and record the result at the end with
`jarvis-jobs finish <id> <0 if done, 1 if not>`.

Your context is only 32K tokens: keep your working set much smaller.
Read files in small parts, grep instead of cat, pipe long command
output through head or tail, and never print a whole log.

You are a small model — stay in your lane:
- Good for you: mechanical, step-by-step jobs — follow a documented
  procedure, collect numbers, write results files, small
  well-specified code changes.
- If a job needs real design work, deep debugging, or you are stuck on
  the same problem after three tries: say so on Discord and end the
  round, and suggest running it on the local model or Sonnet instead.
- Do not open a PR for work you are unsure about; report what you have
  and stop.

Finish the round the usual way: pushed branch, an open PR (or a
Discord note when there is nothing to merge), and a jarvis-discord
notify with the link. Then end your run; I pick what happens next.

STAGE3I_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage3-igpu.txt"

{
cat <<'STAGE3S_EOF'
You are Sonnet, stepping in on a local AI agent build on an Ubuntu
machine that a local model (see docs/local-brain.md) normally runs. I
handed you this round, usually because the local model got stuck.

Work now comes from the queue. This round you do ONE job, given below
after the "YOUR JOB THIS ROUND" line, and then stop. Do not read TASKS.md
(it is history). If no job text follows (an older loop started this
round), run `jarvis-jobs next`, read it with `jarvis-jobs show <id>`, do
that job, and record it at the end with `jarvis-jobs finish <id> <0 or 1>`. Check `GH_TOKEN=$(ghapp-token) gh pr list --state all`,
the branches, and the end of ~/.local/state/jarvis/agent.log if the job
was handed to you after the local model got stuck on it. Leave it in a
state the local model can continue from. For bulk boilerplate you may
delegate to the local model with
`agent-local --dangerously-skip-permissions -p "<task>"` to save API usage.

A job is done only when every acceptance criterion in it passes; the PR
says how each was checked. Finish with a pushed branch, an open PR, and a
jarvis-discord notify with the link and a line on what the local model
should watch out for. Then end your run.

STAGE3S_EOF
printf '%s\n' "$COMMON"
} | agent_write "$JCONF/stage3-sonnet.txt"

# Run the staged agent as the agent user, with the same environment the
# resume service gives it after a reboot.
agent_status=0
# Output is also appended to agent.log, where a later round can read it.
as_agent bash -c 'set -o pipefail; set -a; source "$1"; set +a; cd "$2" && agent-run 2>&1 | tee -a "$3"' \
  _ "$JCONF/agent.env" "$PROJECT_DIR" "$AGENT_HOME/.local/state/jarvis/agent.log" || agent_status=$?
as_agent jarvis-discord notify "Claude Code finished its bootstrap run on $(hostname) (exit $agent_status)." || true
exit "$agent_status"
