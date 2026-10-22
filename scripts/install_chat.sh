#!/usr/bin/env bash
# install_chat.sh — build and install jarvis-chat and jarvis-discord,
# then enable and start the jarvis-chat systemd service.
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

echo "==> Installing systemd unit"
sudo cp systemd/jarvis-chat.service "$SYSTEMD_DIR/jarvis-chat.service"
sudo systemctl daemon-reload
sudo systemctl enable jarvis-chat

echo "==> Starting jarvis-chat"
sudo systemctl restart jarvis-chat
sleep 2

if systemctl is-active --quiet jarvis-chat; then
    echo "==> jarvis-chat is active and running"
else
    echo "==> ERROR: jarvis-chat failed to start" >&2
    journalctl -u jarvis-chat --no-pager -n 20 >&2 || true
    exit 1
fi
