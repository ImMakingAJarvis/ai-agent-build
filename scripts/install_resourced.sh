#!/usr/bin/env bash
# install_resourced.sh — build and install resourced.service (boot + now).
# Same pattern as scripts/install_dashboard.sh.
#
# resourced is the VRAM/RAM ledger + lease broker + swap-window
# controller (plan tasks EVB-25/EVB-27). Host-side since EVB-27: the
# window controller shells out to jarvis-swap, so it needs the host's
# docker, nvidia-smi and the agent job store. It listens on the docker
# bridge gateway (172.17.0.1:9104) so the capability containers reach
# it via host.docker.internal (extra_hosts host-gateway) — see the unit
# for why that is not 127.0.0.1.
#
# Inhibit:  touch /var/lib/jarvis/resourced.disabled
# Undo:     rm /var/lib/jarvis/resourced.disabled && sudo systemctl start resourced
# Also:     sudo systemctl disable --now resourced
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
SYSTEMD_DIR=/etc/systemd/system
NOTES_DIR=/var/lib/jarvis

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:${PATH}"

echo "==> Building resourced"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/resourced" ./cmd/resourced/

echo "==> Installing binary atomically"
if [[ -f "$BIN_DIR/resourced" ]]; then
  sudo cp "$BIN_DIR/resourced" "$BIN_DIR/resourced.bak"
fi
sudo cp "$TMP/resourced" "$BIN_DIR/resourced.tmp"
sudo mv -f "$BIN_DIR/resourced.tmp" "$BIN_DIR/resourced"
sudo chmod 755 "$BIN_DIR/resourced"

echo "==> Inhibit dir (agent owns, nathan can touch disable files)"
sudo mkdir -p "$NOTES_DIR"
sudo chown agent:nathan "$NOTES_DIR"
sudo chmod 2775 "$NOTES_DIR"
sudo mkdir -p /home/agent/.local/state/jarvis
sudo chown agent:agent /home/agent/.local/state/jarvis

echo "==> Installing systemd unit"
sudo cp systemd/resourced.service "$SYSTEMD_DIR/resourced.service"
sudo systemctl daemon-reload
sudo systemctl enable resourced

if [[ -f "$NOTES_DIR/resourced.disabled" ]]; then
  echo "==> $NOTES_DIR/resourced.disabled exists; not starting"
  exit 0
fi

echo "==> (Re)starting resourced"
sudo systemctl restart resourced
sleep 2
if ! curl -sf -m 3 "http://172.17.0.1:9104/state" > /dev/null; then
  echo "!! resourced is not answering on 172.17.0.1:9104 — journal: "
  sudo journalctl -u resourced -n 20 --no-pager
  exit 1
fi
echo "==> resourced up on 172.17.0.1:9104"
