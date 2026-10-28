#!/usr/bin/env bash
# install_memory.sh — install the Jarvis Memory Service (local vector
# database for work logs) and its periodic reindex timer.
#
# The embedding model (all-MiniLM-L6-v2 ONNX) is downloaded separately by
# download_memory_model.sh; without it the service runs but /health reports
# model:false and /search returns 503.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSTEMD_DIR=/etc/systemd/system

echo "==> Installing systemd units"
sudo cp "$REPO_ROOT/systemd/jarvis-memory.service" "$SYSTEMD_DIR/"
sudo cp "$REPO_ROOT/systemd/jarvis-memory-reindex.service" "$SYSTEMD_DIR/"
sudo cp "$REPO_ROOT/systemd/jarvis-memory-reindex.timer" "$SYSTEMD_DIR/"
sudo systemctl daemon-reload
sudo systemctl enable --now jarvis-memory.service
sudo systemctl enable --now jarvis-memory-reindex.timer

echo "==> Status"
systemctl is-active jarvis-memory.service
systemctl is-active jarvis-memory-reindex.timer
echo "==> Health"
sleep 2
curl -sf http://127.0.0.1:8101/health && echo || echo "    (service starting or model missing — see journalctl -u jarvis-memory)"
