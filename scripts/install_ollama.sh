#!/usr/bin/env bash
# Install Ollama as a systemd service (enabled at boot) and verify it is
# listening on localhost:11434 and new enough for /v1/messages (>= 0.14.0).
set -euo pipefail

MIN=0.14.0
if ! command -v ollama >/dev/null; then
  curl -fsSL https://ollama.com/install.sh | sh
fi
sudo systemctl enable --now ollama

ver=$(ollama --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
if [[ "$(printf '%s\n%s\n' "$MIN" "$ver" | sort -V | head -1)" != "$MIN" ]]; then
  echo "ollama $ver is older than $MIN (no Anthropic API); rerun the installer" >&2
  exit 1
fi

for _ in $(seq 30); do
  curl -fsS http://localhost:11434/api/version >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://localhost:11434/api/version || { echo "ollama not listening on :11434" >&2; exit 1; }
echo
systemctl is-enabled ollama
echo "ollama $ver OK"
