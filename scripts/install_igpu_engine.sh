#!/usr/bin/env bash
# install_igpu_engine: build a Vulkan llama.cpp for the Ryzen iGPU and
# install the jarvis-engine-igpu unit (disabled — the default runtime is
# the docker-compose container `engine-igpu`, which bind-mounts this
# build from /opt/jarvis-igpu).
#
#   install_igpu_engine.sh              build + install (unit stays disabled)
#   install_igpu_engine.sh --build-only just the build, no systemd/group changes
#
# Result:
#   /opt/jarvis-igpu/bin/llama-server   Vulkan build (sees both GPUs)
#   /opt/jarvis-igpu/bin/llama-bench    benchmark tool
#   /etc/systemd/system/jarvis-engine-igpu.service  (disabled; rollback path)
#
# The iGPU is device Vulkan1 on this machine; the unit pins -dev Vulkan1.
# In the container there is no NVIDIA Vulkan ICD, so the iGPU is the only
# device and the compose command pins -dev Vulkan0 instead.
# See docs/igpu-model-handler.md for the investigation this came from.
set -euo pipefail

BUILD_ONLY=0
[[ "${1:-}" == "--build-only" ]] && BUILD_ONLY=1

# Overridable so the Dory build (docs/igpu-model-handler.md) is reproducible:
#   LLAMA_REPO=https://github.com/brianchmielNV/llama.cpp \
#   LLAMA_REF=dory-support LLAMA_SRC=/opt/jarvis-igpu/src-dory \
#   LLAMA_BUILD=/opt/jarvis-igpu/build-dory LLAMA_BIN=/opt/jarvis-igpu/bin-dory \
#   install_igpu_engine.sh --build-only
SRC="${LLAMA_SRC:-/opt/jarvis-igpu/src}"
DEST=/opt/jarvis-igpu
BUILD="${LLAMA_BUILD:-/opt/jarvis-igpu/build}"
BIN="${LLAMA_BIN:-/opt/jarvis-igpu/bin}"
LLAMA_REPO="${LLAMA_REPO:-https://github.com/ggml-org/llama.cpp}"
LLAMA_REF="${LLAMA_REF:-}"

log() { echo "==> $*"; }

########################################
# 1. Build dependencies
########################################
log "Installing build dependencies (glslc, SPIRV-Headers) ..."
sudo apt-get install -y glslc cmake g++

# SPIRV-Headers has no Ubuntu package; install the header-only repo.
if [[ ! -f /usr/local/share/cmake/SPIRV-Headers/SPIRV-HeadersConfig.cmake ]]; then
  log "Installing SPIRV-Headers ..."
  if [[ ! -d /tmp/SPIRV-Headers ]]; then
    git clone --depth 1 https://github.com/KhronosGroup/SPIRV-Headers /tmp/SPIRV-Headers
  fi
  cmake -S /tmp/SPIRV-Headers -B /tmp/SPIRV-Headers-build -DSPIRV_HEADERS_ENABLE_TESTS=OFF > /dev/null
  sudo cmake --install /tmp/SPIRV-Headers-build > /dev/null
fi

########################################
# 2. Source + build (Vulkan only; CUDA stays with the 4090 engine)
########################################
if [[ ! -d $SRC ]]; then
  log "Cloning $LLAMA_REPO${LLAMA_REF:+ (ref: $LLAMA_REF)} ..."
  git clone --depth 1 ${LLAMA_REF:+"--branch" "$LLAMA_REF"} "$LLAMA_REPO" "$SRC"
fi
log "Building (Vulkan) ..."
cmake -S "$SRC" -B "$BUILD" -DCMAKE_BUILD_TYPE=Release \
  -DGGML_VULKAN=ON -DGGML_CUDA=OFF -DLLAMA_CURL=OFF -DBUILD_SHARED_LIBS=OFF > /dev/null
cmake --build "$BUILD" --config Release -j"$(nproc)" \
  --target llama-server llama-bench
mkdir -p "$BIN"
cp "$BUILD"/bin/llama-server "$BUILD"/bin/llama-bench "$BIN/"
# The service and model dir are agent-owned: the server runs as agent and
# the model is swapped in by hand.
sudo chown -R agent:agent "$BIN"
sudo install -d -o agent -g agent "$DEST/models"
log "Installed to $BIN"

if [[ $BUILD_ONLY -eq 1 ]]; then
  log "--build-only: done"
  exit 0
fi

########################################
# 3. Group access for the agent user (iGPU render node)
########################################
log "Ensuring agent is in video/render groups ..."
sudo usermod -aG video,render agent

########################################
# 4. Unit (disabled — the container is the default runtime; the unit is
#    the rollback path, enable with: sudo systemctl enable --now jarvis-engine-igpu)
########################################
sudo install -m 644 systemd/jarvis-engine-igpu.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl disable jarvis-engine-igpu 2>/dev/null || true
log "Unit installed but DISABLED (rollback path). Model: put a GGUF at /opt/jarvis-igpu/models/jarvis-igpu.gguf, then run the container:"
log "  docker compose -p ai-agent-build -f docker-compose.yml up -d engine-igpu"
log "  (or scripts/install_helper.sh, which also deploys the jarvis-helper CLI)"
