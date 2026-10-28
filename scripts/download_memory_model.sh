#!/usr/bin/env bash
# download_memory_model.sh — fetch all-MiniLM-L6-v2 (ONNX) from ModelScope
# into $JARVIS_DATA_DIR/memory/model (~90 MB).
#
# ModelScope is used because HuggingFace blocks datacenter IPs without
# credentials; the same repo (sentence-transformers/all-MiniLM-L6-v2) is
# mirrored there with an ONNX export.
set -euo pipefail

DATA_DIR="${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}"
MODEL_DIR="$DATA_DIR/memory/model"
BASE="https://modelscope.cn/api/v1/models/sentence-transformers/all-MiniLM-L6-v2/repo?Revision=master&FilePath"

mkdir -p "$MODEL_DIR"
cd "$MODEL_DIR"

for f in "onnx/model.onnx:model.onnx" "tokenizer.json:tokenizer.json" \
         "config.json:config.json" "1_Pooling/config.json:pooling.json"; do
  src="${f%%:*}"
  dst="${f##*:}"
  if [ -s "$dst" ]; then
    echo "    $dst present"
    continue
  fi
  echo "==> downloading $dst"
  curl -sfL --retry 3 -o "$dst" "$BASE=$src"
done

echo "==> model ready at $MODEL_DIR"
ls -la "$MODEL_DIR"
