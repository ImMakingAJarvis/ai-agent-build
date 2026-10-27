#!/usr/bin/env bash
# install_claude_md.sh — install the agent's own CLAUDE.md
# (docs/agent-claude.md) as the user-level memory file ~/.claude/CLAUDE.md,
# which Claude Code loads in every session on top of the repo's CLAUDE.md.
# Re-run after updating docs/agent-claude.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$REPO_ROOT/docs/agent-claude.md"
DEST="$HOME/.claude/CLAUDE.md"

[ -f "$SRC" ] || { echo "missing $SRC" >&2; exit 1; }
mkdir -p "$(dirname "$DEST")"

# Atomic replace so a session mid-read never sees a half-written file.
TMP="$(mktemp)"
cp "$SRC" "$TMP"
mv -f "$TMP" "$DEST"
echo "installed $DEST (from docs/agent-claude.md)"
