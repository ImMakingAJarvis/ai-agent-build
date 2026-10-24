#!/usr/bin/env bash
# install_dashboard.sh — build and install jarvis-dashboard.service (boot + now).
# Same pattern as scripts/install_chat.sh / install_agent_run.sh.
#
# Inhibit:  touch /var/lib/jarvis/dashboard.disabled
# Undo:     rm /var/lib/jarvis/dashboard.disabled && sudo systemctl start jarvis-dashboard
# Also:     sudo systemctl disable --now jarvis-dashboard
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
SYSTEMD_DIR=/etc/systemd/system
NOTES_DIR=/var/lib/jarvis

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:${PATH}"

echo "==> Building jarvis-dashboard"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/jarvis-dashboard" ./cmd/dashboard/

echo "==> Installing binary atomically"
sudo cp "$TMP/jarvis-dashboard" "$BIN_DIR/jarvis-dashboard.tmp"
sudo mv -f "$BIN_DIR/jarvis-dashboard.tmp" "$BIN_DIR/jarvis-dashboard"
sudo chmod 755 "$BIN_DIR/jarvis-dashboard"

echo "==> Inhibit dir (agent owns, nathan can touch disable files)"
sudo mkdir -p "$NOTES_DIR"
sudo chown agent:nathan "$NOTES_DIR"
sudo chmod 2775 "$NOTES_DIR"
sudo mkdir -p /home/agent/.local/state/jarvis
sudo chown agent:agent /home/agent/.local/state/jarvis

echo "==> Installing systemd unit"
sudo cp systemd/jarvis-dashboard.service "$SYSTEMD_DIR/jarvis-dashboard.service"
sudo systemctl daemon-reload
sudo systemctl enable jarvis-dashboard

if [[ -f "$NOTES_DIR/dashboard.disabled" ]]; then
  echo "==> $NOTES_DIR/dashboard.disabled exists; not starting"
  exit 0
fi
if ss -ltn 2>/dev/null | grep -q ':8080'; then
  echo "==> :8080 already listening; enabled for next boot, not starting a second copy"
  exit 0
fi

echo "==> Starting jarvis-dashboard"
sudo systemctl start jarvis-dashboard
sleep 2
if systemctl is-active --quiet jarvis-dashboard; then
  echo "==> jarvis-dashboard is active on :8080"
else
  echo "==> ERROR: jarvis-dashboard failed to start" >&2
  journalctl -u jarvis-dashboard --no-pager -n 30 >&2 || true
  exit 1
fi
