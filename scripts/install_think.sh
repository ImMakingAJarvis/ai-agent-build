#!/usr/bin/env bash
# install_think.sh — install the idle-work think queue (docs/idle-work.md).
#
# Same pattern as scripts/install_agent_run.sh (system units, User=agent,
# atomic copies). The units run the scripts straight from the repo tree,
# so a re-run of this installer is also the "script update" path.
#
# Undo:  sudo systemctl disable --now jarvis-think-topup.timer jarvis-think-digest.timer
# Status: systemctl list-timers 'jarvis-think*' ; journalctl -u jarvis-think-topup
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSTEMD_DIR=/etc/systemd/system

cd "$REPO_ROOT"

echo "==> Syntax-checking the scripts"
python3 -m py_compile scripts/think_topup.py scripts/think_digest.py

echo "==> Installing systemd units"
for u in jarvis-think-topup.service jarvis-think-topup.timer \
         jarvis-think-digest.service jarvis-think-digest.timer; do
  sudo cp "systemd/$u" "$SYSTEMD_DIR/$u.tmp"
  sudo mv -f "$SYSTEMD_DIR/$u.tmp" "$SYSTEMD_DIR/$u"
done

sudo systemctl daemon-reload

echo "==> Enabling the timers"
sudo systemctl enable --now jarvis-think-topup.timer
sudo systemctl enable --now jarvis-think-digest.timer

echo "==> State"
systemctl list-timers 'jarvis-think*' --no-pager || true
echo "done — the loop activates on the next top-up tick (15 min)."
