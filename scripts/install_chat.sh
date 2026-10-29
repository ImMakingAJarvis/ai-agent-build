#!/usr/bin/env bash
# install_chat.sh — build and install jarvis-chat and jarvis-discord, then
# manage the jarvis-chat systemd unit (the fallback for the Docker chat).
#
# The Docker container is the primary chat daemon (docs/services.md). The
# host unit must never run at the same time: two daemons with the same bot
# token both receive every owner DM, so they double-reply and every
# !build/!fix gets enqueued twice. This script therefore leaves the unit
# disabled while the chat container is running, and only enables it as a
# fallback when the container is down.
#
# Binaries are installed atomically: copied to a temp name, then mv'd
# into place so a running process is never truncated.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
SYSTEMD_DIR=/etc/systemd/system

cd "$REPO_ROOT"

TMPDIR_BUILD="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_BUILD"' EXIT

echo "==> Building jarvis-chat and jarvis-discord"
go build -o "$TMPDIR_BUILD/jarvis-chat" ./cmd/jarvis-chat/
go build -o "$TMPDIR_BUILD/jarvis-discord" ./cmd/jarvis-discord/

echo "==> Installing binaries atomically"
sudo cp "$TMPDIR_BUILD/jarvis-chat" "$BIN_DIR/jarvis-chat.tmp"
sudo mv -f "$BIN_DIR/jarvis-chat.tmp" "$BIN_DIR/jarvis-chat"
sudo cp "$TMPDIR_BUILD/jarvis-discord" "$BIN_DIR/jarvis-discord.tmp"
sudo mv -f "$BIN_DIR/jarvis-discord.tmp" "$BIN_DIR/jarvis-discord"
echo "    $BIN_DIR/jarvis-chat installed"
echo "    $BIN_DIR/jarvis-discord installed"

echo "==> Installing agent-run atomically"
sudo cp "$REPO_ROOT/scripts/agent-run" "$BIN_DIR/agent-run.tmp"
sudo mv -f "$BIN_DIR/agent-run.tmp" "$BIN_DIR/agent-run"
echo "    $BIN_DIR/agent-run installed"

echo "==> Installing chat system prompt (live override: \$JARVIS_CONF/chat-prompt.txt)"
sudo mkdir -p /home/agent/.config/jarvis
sudo cp "$REPO_ROOT/cmd/jarvis-chat/system_prompt.txt" /home/agent/.config/jarvis/chat-prompt.txt
sudo chown agent:agent /home/agent/.config/jarvis/chat-prompt.txt

echo "==> Installing systemd unit"
sudo cp systemd/jarvis-chat.service "$SYSTEMD_DIR/jarvis-chat.service"
sudo systemctl daemon-reload

if docker inspect --format '{{.State.Running}}' jarvis-chat 2>/dev/null | grep -q '^true$'; then
    # Container is the primary chat: keep the host unit disabled and stopped
    # (this also repairs a system where both were left running).
    echo "==> chat container is running; host unit stays disabled (no double bot)"
    sudo systemctl disable --now jarvis-chat
else
    echo "==> chat container not running; enabling host fallback"
    sudo systemctl enable jarvis-chat
    sudo systemctl restart jarvis-chat
    sleep 2
    if systemctl is-active --quiet jarvis-chat; then
        echo "==> jarvis-chat is active and running"
    else
        echo "==> ERROR: jarvis-chat failed to start" >&2
        journalctl -u jarvis-chat --no-pager -n 20 >&2 || true
        exit 1
    fi
fi
