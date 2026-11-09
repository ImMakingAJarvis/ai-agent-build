#!/usr/bin/env bash
# install_photo.sh — build and install jarvis-photo, the code executor that
# runs a queued photo job (cmd/jarvis-photo, internal/photo).
#
# A photo-tagged job runs as code, with no model round (not the brain, not
# the iGPU helper): the agentjob Runner runs this executor directly. It is
# self-sufficient — it reads the job's (brain-expanded) prompt, starts the
# art service if it is down, waits for the model to load, generates the
# image on the art service (the 4090 is free during the brain-swap window;
# the PNG lands on disk and in the gallery at the same time), delivers the
# finished image to the owner's Discord DM, and queues the job's optional
# follow-up as a new job with the image path.
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
