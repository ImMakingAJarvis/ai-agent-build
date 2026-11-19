#!/usr/bin/env bash
# install_mq.sh — build and install the jarvis-mq CLI to /usr/local/bin and
# sync the capability catalog to the machine-wide mirror
# /etc/jarvis/capabilities.yaml (0644). Same pattern as
# scripts/install_kanban_cli.sh.
#
# The mirror is what makes sync RPC (`jarvis-mq call`) usable for local
# users other than the agent: the refusal gate (EVB-38) is fail-closed, and
# the repo catalog lives under /home/agent, which nobody else can traverse
# (Nathan's !fix "I still can't RPC successfully anymore", 2026-11-19).
# catalogDefaultPath falls back to the mirror when the repo path is not
# reachable; the agent keeps reading the repo copy (the source of truth).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin
MIRROR_DIR=/etc/jarvis

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:${PATH}"

echo "==> Building jarvis-mq"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/jarvis-mq" ./cmd/jarvis-mq/

echo "==> Installing binary atomically"
sudo cp "$TMP/jarvis-mq" "$BIN_DIR/jarvis-mq.tmp"
sudo mv -f "$BIN_DIR/jarvis-mq.tmp" "$BIN_DIR/jarvis-mq"
sudo chmod 755 "$BIN_DIR/jarvis-mq"

echo "==> Syncing capability catalog to $MIRROR_DIR/capabilities.yaml"
sudo install -d -m 755 "$MIRROR_DIR"
sudo cp "$REPO_ROOT/config/capabilities.yaml" "$MIRROR_DIR/capabilities.yaml.tmp"
sudo mv -f "$MIRROR_DIR/capabilities.yaml.tmp" "$MIRROR_DIR/capabilities.yaml"
sudo chmod 644 "$MIRROR_DIR/capabilities.yaml"

echo "==> jarvis-mq installed (catalog mirror at $MIRROR_DIR/capabilities.yaml)"
