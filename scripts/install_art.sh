#!/usr/bin/env bash
# Deploy the art service code to the pinned deploy point /opt/jarvis-art
# and download the Z-Image-Turbo weights to /var/lib/jarvis/art/model.
#
#   scripts/install_art.sh            # deploy code + fetch model if absent
#   ART_MODEL_REPO=<repo> scripts/install_art.sh   # override the HF repo
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY=/opt/jarvis-art
DATA=/var/lib/jarvis/art
# Official Tongyi-MAI pipeline repo (model_index.json + subfolders); the
# T5B FP8 port is single-file transformer weights only and is NOT a loadable
# diffusers pipeline (no model_index.json).
MODEL_REPO="${ART_MODEL_REPO:-Tongyi-MAI/Z-Image-Turbo}"

mkdir -p "$DATA/model" "$DATA/out"
if [[ ! -d "$DEPLOY" ]]; then
  sudo mkdir -p "$DEPLOY" && sudo chown agent:agent "$DEPLOY"
fi
install -m 644 "$REPO/scripts/art_service.py" "$DEPLOY/art_service.py"
echo "DEPLOYED_FROM $REPO @ $(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo nogit) $(date -Is)" \
  >"$DEPLOY/DEPLOYED_FROM"
echo "==> Deployed art service to $DEPLOY"

if [[ -f "$DATA/model/model_index.json" ]]; then
  echo "==> Model already present in $DATA/model; skipping download"
else
  echo "==> Downloading $MODEL_REPO -> $DATA/model"
  python3 - "$MODEL_REPO" "$DATA/model" <<'EOF'
import sys

from huggingface_hub import snapshot_download

path = snapshot_download(repo_id=sys.argv[1], local_dir=sys.argv[2])
print("downloaded to", path)
EOF
fi

echo "==> Start the service: docker compose up -d art"
