#!/usr/bin/env bash
# install_helper: deploy the jarvis-helper CLI + setup context, and make
# the iGPU engine always-on.
#
#   install_helper.sh    deploy CLI + context, enable jarvis-engine-igpu
#
# Re-run after editing scripts/jarvis-helper or
# scripts/jarvis-helper-context.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> Installing jarvis-helper CLI"
sudo install -m 755 "$REPO_ROOT/scripts/jarvis-helper" /usr/local/bin/jarvis-helper

echo "==> Installing setup context"
sudo install -d /opt/jarvis-helper
sudo install -m 644 "$REPO_ROOT/scripts/jarvis-helper-context.md" /opt/jarvis-helper/context.md

echo "==> Ensuring iGPU engine model is present"
if [[ ! -s /opt/jarvis-igpu/models/jarvis-igpu.gguf ]]; then
  echo "    MISSING: put the helper GGUF at /opt/jarvis-igpu/models/jarvis-igpu.gguf"
  echo "    (current pick: Qwen/Qwen2.5-3B-Instruct-GGUF q4_k_m, ~2 GB)"
  echo "    https://huggingface.co/Qwen/Qwen2.5-3B-Instruct-GGUF/resolve/main/qwen2.5-3b-instruct-q4_k_m.gguf"
fi

echo "==> Enabling jarvis-engine-igpu (always-on)"
sudo systemctl enable --now jarvis-engine-igpu

echo "==> Health"
for _ in $(seq 1 30); do
  if curl -s --max-time 3 http://127.0.0.1:11435/health 2>/dev/null | grep -q ok; then
    echo "    engine is up on 127.0.0.1:11435"
    break
  fi
  sleep 2
done

jarvis-helper --status
