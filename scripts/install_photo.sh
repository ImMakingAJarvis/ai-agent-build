#!/usr/bin/env bash
# install_photo.sh — build and install jarvis-photo, the executor the iGPU
# round runs for a queued photo job (cmd/jarvis-photo, internal/photo).
#
# The round that the queue watcher runs for a photo-tagged job does exactly
# one thing: run this. It reads the job's (brain-expanded) prompt, generates
# the image on the art service (the 4090 is free during the brain-swap
# window) and delivers the finished PNG to the owner's Discord DM.
#
# Binaries are installed atomically: copied to a temp name, then mv'd into
# place so a running process is never truncated.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_DIR=/usr/local/bin

cd "$REPO_ROOT"

TMPDIR_BUILD="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_BUILD"' EXIT

echo "==> Building jarvis-photo"
go build -o "$TMPDIR_BUILD/jarvis-photo" ./cmd/jarvis-photo/

echo "==> Installing jarvis-photo atomically"
sudo cp "$TMPDIR_BUILD/jarvis-photo" "$BIN_DIR/jarvis-photo.tmp"
sudo mv -f "$BIN_DIR/jarvis-photo.tmp" "$BIN_DIR/jarvis-photo"
echo "    $BIN_DIR/jarvis-photo installed"
