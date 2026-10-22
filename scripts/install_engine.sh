#!/usr/bin/env bash
# Install llama.cpp server under /opt/jarvis-engine with CUDA support.
# Idempotent: safe to run multiple times; skips steps already done.
# No service, no model yet — just the binary and build deps.
#
# Usage: sudo bash scripts/install_engine.sh [--model]
#   --model  Also download the brain GGUF to /opt/jarvis-engine/models/
set -euo pipefail

TAG=b11815
SRC=/tmp/llama.cpp-build
DEST=/opt/jarvis-engine
BIN="$DEST/bin/llama-server"
INSTALL_MODEL="${1:-}"

########################################
# 1. Build dependencies
########################################
install_if() { command -v "$1" >/dev/null 2>&1 || sudo apt-get install -y "$2"; }

install_if cmake cmake
install_if git git
install_if build-essential build-essential

########################################
# 2. Source checkout (skip if up to date)
########################################
if [[ -d "$SRC/.git" ]]; then
  cd "$SRC"
  # Fetch and reset to the pinned tag
  git fetch --tags --force
  git checkout --force "$TAG"
else
  echo "==> Cloning llama.cpp $TAG ..."
  mkdir -p "$SRC"
  git clone --depth 1 --branch "$TAG" https://github.com/ggml-org/llama.cpp.git "$SRC"
  cd "$SRC"
fi

########################################
# 3. Build with CUDA
########################################
# nvcc lives in /usr/local/cuda/bin but may not be in PATH
export PATH="/usr/local/cuda/bin:$PATH"

if [[ -n "${FORCE_BUILD:-}" ]] || [[ ! -f "$BIN" ]]; then
  echo "==> Building llama.cpp server (CUDA) ..."
  mkdir -p build && cd build
  cmake -DBUILD_SHARED_LIBS=OFF \
        -DGGML_CUDA=ON \
        -DLLAMA_SERVER_LOGS=ON \
        ..
  cmake --build . --config Release -j "$(nproc)"
  cd -
else
  echo "==> Binary already at $BIN — skipping build (pass FORCE_BUILD=1 to rebuild)"
fi

########################################
# 4. Install to /opt/jarvis-engine
########################################
sudo mkdir -p "$DEST/bin" "$DEST/models"
sudo cp "$SRC/build/bin/llama-server" "$BIN"
sudo chmod 755 "$BIN"

echo
echo "==> Installed: $BIN"
"$BIN" --help | head -1

########################################
# 5. Optional: download brain model
########################################
if [[ "$INSTALL_MODEL" == "--model" ]]; then
  echo
  echo "==> Locating GGUF for jarvis-brain ..."
  # Ollama may store blobs in several places depending on install method
  MANIFEST=$(ollama show --modelfile jarvis-brain 2>/dev/null | grep -oP 'sha256-\K[a-f0-9]+' | head -1 || true)

  if [[ -n "$MANIFEST" ]]; then
    HASH="$MANIFEST"
    BLOB=""
    for BDIR in /usr/share/ollama/.ollama/models/blobs "${XDG_DATA_HOME:-$HOME/.ollama}/models/blobs"; do
      [[ -f "$BDIR/$HASH" ]] && BLOB="$BDIR/$HASH" && break
    done
    if [[ -f "$BLOB" ]]; then
      sudo cp "$BLOB" "$DEST/models/jarvis-brain.gguf"
      echo "==> Copied brain GGUF to $DEST/models/jarvis-brain.gguf ($(du -h "$DEST/models/jarvis-brain.gguf" | cut -f1))"
    else
      echo "==> WARNING: GGUF blob $HASH not found in $BLOB_DIR" >&2
    fi
  else
    echo "==> WARNING: could not determine jarvis-brain GGUF hash" >&2
  fi
fi

echo "==> Done."
