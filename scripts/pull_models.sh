#!/usr/bin/env bash
# Pull the light and heavy models via Ollama, then create a Modelfile for
# jarvis-brain that pins it in VRAM (num_gpu -1 so all experts stay on GPU).
set -euo pipefail

source "${HOME}/.config/jarvis/local.env" 2>/dev/null || true

LIGHT_MODEL="${LOCAL_MODEL:-qwen3.6:27b}"
HEAVY_MODEL="${LOCAL_CODER_MODEL:-}"

echo "==> Pulling light model: $LIGHT_MODEL"
ollama pull "$LIGHT_MODEL"

if [[ -n "$HEAVY_MODEL" ]]; then
  echo "==> Pulling heavy model: $HEAVY_MODEL"
  ollama pull "$HEAVY_MODEL"
else
  echo "==> No heavy model configured (LOCAL_CODER_MODEL is empty); skipping"
fi

# Create Modelfile for the light model — pin fully in VRAM.
MODFILE="${HOME}/.config/jarvis/Modelfile.brain"
cat >"$MODFILE" <<EOF
FROM ${LIGHT_MODEL}

# Pin the entire model in GPU VRAM so it never swaps out.
PARAMETER num_gpu -1
EOF
echo "==> Wrote Modelfile for jarvis-brain to $MODFILE"

# Create the jarvis-brain model if it doesn't exist yet.
if ! ollama list --format '{{.Name}}' | grep -q '^jarvis-brain:latest$'; then
  echo "==> Creating jarvis-brain from Modelfile"
  cd "$HOME/.config/jarvis" && ollama create jarvis-brain -f Modelfile.brain
fi

# Verify both respond.
echo "==> Verifying light model responds..."
ollama run "$LIGHT_MODEL" "Reply with OK" --quiet 2>/dev/null || {
  echo "ERROR: light model did not respond" >&2
  exit 1
}

if [[ -n "$HEAVY_MODEL" ]]; then
  echo "==> Verifying heavy model responds..."
  ollama run "$HEAVY_MODEL" "Reply with OK" --quiet 2>/dev/null || {
    echo "ERROR: heavy model did not respond" >&2
    exit 1
  }
fi

echo "==> All models OK"
