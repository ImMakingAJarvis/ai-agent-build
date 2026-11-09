#!/usr/bin/env bash
# Download the FLUX.2 [klein] 4B weights — the reference-capable image model
# the art service lazy-loads on the first reference generation (uploads or a
# job's reference sidecar route there; plain photos never pay for it).
#
#   scripts/install_klein.sh             # fetch if absent
#   ART_KLEIN_REPO=<repo> scripts/install_klein.sh   # override the HF repo
#   ART_KLEIN_DIR=<dir> scripts/install_klein.sh     # override the target
set -euo pipefail

DATA=/var/lib/jarvis/art
# Official BFL pipeline repo (model_index.json + transformer/text_encoder/VAE).
MODEL_REPO="${ART_KLEIN_REPO:-black-forest-labs/FLUX.2-klein-4B}"
KLEIN_DIR="${ART_KLEIN_DIR:-$DATA/models/flux2-klein-4b}"

if [[ -f "$KLEIN_DIR/model_index.json" ]]; then
  echo "==> Weights already present in $KLEIN_DIR; skipping download"
  exit 0
fi

echo "==> Downloading $MODEL_REPO -> $KLEIN_DIR"
python3 - "$MODEL_REPO" "$KLEIN_DIR" <<'EOF'
import sys

from huggingface_hub import snapshot_download

path = snapshot_download(repo_id=sys.argv[1], local_dir=sys.argv[2])
print("downloaded to", path)
EOF
echo "==> Done; the art service picks the weights up on the next reference generation"
