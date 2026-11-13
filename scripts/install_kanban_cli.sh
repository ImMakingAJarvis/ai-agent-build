#!/usr/bin/env bash
# install_kanban_cli.sh — build and install the jarvis-kanban CLI (kanban
# comments: show a task with its discussion, post a comment) to /usr/local/bin.
# Same pattern as scripts/install_dashboard.sh.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:${PATH}"

echo "==> Building jarvis-kanban"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/jarvis-kanban" ./cmd/jarvis-kanban/

echo "==> Installing binary atomically"
sudo cp "$TMP/jarvis-kanban" "$BIN_DIR/jarvis-kanban.tmp"
sudo mv -f "$BIN_DIR/jarvis-kanban.tmp" "$BIN_DIR/jarvis-kanban"
sudo chmod 755 "$BIN_DIR/jarvis-kanban"

echo "==> jarvis-kanban installed"
