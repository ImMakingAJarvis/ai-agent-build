#!/usr/bin/env bash
# install_watchdog.sh — install jarvis-watchdog.service so the stack failsafe
# starts on boot. Same pattern as scripts/install_agent_run.sh.
#
# Inhibit without disable:  touch /var/lib/jarvis/jarvis-watchdog.disabled
# Undo:                     rm /var/lib/jarvis/jarvis-watchdog.disabled && sudo systemctl start jarvis-watchdog
# Also:                     sudo systemctl disable --now jarvis-watchdog
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
SYSTEMD_DIR=/etc/systemd/system
NOTES_DIR=/var/lib/jarvis

cd "$REPO_ROOT"

echo "==> Installing jarvis-watchdog script atomically"
sudo cp "$REPO_ROOT/scripts/jarvis-watchdog" "$BIN_DIR/jarvis-watchdog.tmp"
sudo mv -f "$BIN_DIR/jarvis-watchdog.tmp" "$BIN_DIR/jarvis-watchdog"
sudo chmod 755 "$BIN_DIR/jarvis-watchdog"

echo "==> Notes/inhibit dir (agent owns, nathan can touch the disable file)"
sudo mkdir -p "$NOTES_DIR"
sudo chown agent:nathan "$NOTES_DIR"
sudo chmod 2775 "$NOTES_DIR"

echo "==> Installing systemd unit"
sudo cp systemd/jarvis-watchdog.service "$SYSTEMD_DIR/jarvis-watchdog.service"
sudo systemctl daemon-reload
sudo systemctl enable jarvis-watchdog

if [[ -f "$NOTES_DIR/jarvis-watchdog.disabled" ]]; then
  echo "==> $NOTES_DIR/jarvis-watchdog.disabled exists; not starting"
  exit 0
fi
if pgrep -f '/usr/local/bin/jarvis-watchdog' >/dev/null; then
  echo "==> jarvis-watchdog already running; enabled for next boot, not starting a second copy"
  exit 0
fi

echo "==> Starting jarvis-watchdog"
sudo systemctl start jarvis-watchdog
sleep 2
if systemctl is-active --quiet jarvis-watchdog; then
  echo "==> jarvis-watchdog is active"
else
  echo "==> ERROR: jarvis-watchdog failed to start" >&2
  journalctl -u jarvis-watchdog --no-pager -n 30 >&2 || true
  exit 1
fi
