#!/usr/bin/env bash
# install_agent_run.sh — install agent-run.service so coding rounds start on boot.
# Same pattern as scripts/install_chat.sh (system unit, User=agent, atomic bin copy).
#
# Inhibit without disable:  touch /var/lib/jarvis/agent-run.disabled
# Undo:                     rm /var/lib/jarvis/agent-run.disabled && sudo systemctl start agent-run
# Also:                     sudo systemctl disable --now agent-run
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
SYSTEMD_DIR=/etc/systemd/system
NOTES_DIR=/var/lib/jarvis

cd "$REPO_ROOT"

echo "==> Installing agent-run script atomically"
sudo cp "$REPO_ROOT/scripts/agent-run" "$BIN_DIR/agent-run.tmp"
sudo mv -f "$BIN_DIR/agent-run.tmp" "$BIN_DIR/agent-run"
sudo chmod 755 "$BIN_DIR/agent-run"

echo "==> Installing agent-local atomically"
sudo cp "$REPO_ROOT/scripts/agent-local" "$BIN_DIR/agent-local.tmp"
sudo mv -f "$BIN_DIR/agent-local.tmp" "$BIN_DIR/agent-local"
sudo chmod 755 "$BIN_DIR/agent-local"

echo "==> Installing agent-run output-token drop-in (no restart)"
sudo mkdir -p "$SYSTEMD_DIR/agent-run.service.d"
sudo cp systemd/agent-run.service.d/output-tokens.conf \
  "$SYSTEMD_DIR/agent-run.service.d/output-tokens.conf"

echo "==> Notes/inhibit dir (agent owns, nathan can touch the disable file)"
sudo mkdir -p "$NOTES_DIR"
sudo chown agent:nathan "$NOTES_DIR"
sudo chmod 2775 "$NOTES_DIR"

echo "==> Installing systemd unit"
sudo cp systemd/agent-run.service "$SYSTEMD_DIR/agent-run.service"
sudo systemctl daemon-reload
sudo systemctl enable agent-run

if [[ -f "$NOTES_DIR/agent-run.disabled" ]]; then
  echo "==> $NOTES_DIR/agent-run.disabled exists; not starting"
  exit 0
fi
if pgrep -f '/usr/local/bin/agent-run' >/dev/null; then
  echo "==> agent-run already running; enabled for next boot, not starting a second copy"
  exit 0
fi

echo "==> Starting agent-run"
sudo systemctl start agent-run
sleep 2
if systemctl is-active --quiet agent-run; then
  echo "==> agent-run is active"
else
  echo "==> ERROR: agent-run failed to start" >&2
  journalctl -u agent-run --no-pager -n 30 >&2 || true
  exit 1
fi
