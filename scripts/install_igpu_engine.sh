#!/usr/bin/env bash
# install_igpu_engine: build a Vulkan llama.cpp for the Ryzen iGPU and
# install the jarvis-engine-igpu service (disabled by default).
#
#   install_igpu_engine.sh              build + install (service stays disabled)
#   install_igpu_engine.sh --build-only just the build, no systemd/group changes
#
# Result:
#   /opt/jarvis-igpu/bin/llama-server   Vulkan build (sees both GPUs)
#   /opt/jarvis-igpu/bin/llama-bench    benchmark tool
#   /etc/systemd/system/jarvis-engine-igpu.service  (disabled)
#
# The iGPU is device Vulkan1 on this machine; the unit pins -dev Vulkan1.
# See docs/igpu-model-handler.md for the investigation this came from.
set -euo pipefail

BUILD_ONLY=0
[[ "${1:-}" == "--build-only" ]] && BUILD_ONLY=1

SRC=/opt/jarvis-igpu/src
DEST=/opt/jarvis-igpu
BUILD=$DEST/build

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
  log "Cloning llama.cpp ..."
  git clone --depth 1 https://github.com/ggml-org/llama.cpp "$SRC"
fi
log "Building (Vulkan) ..."
cmake -S "$SRC" -B "$BUILD" -DCMAKE_BUILD_TYPE=Release \
  -DGGML_VULKAN=ON -DGGML_CUDA=OFF -DLLAMA_CURL=OFF -DBUILD_SHARED_LIBS=OFF > /dev/null
cmake --build "$BUILD" --config Release -j"$(nproc)" \
  --target llama-server llama-bench
mkdir -p "$DEST/bin"
cp "$BUILD"/bin/llama-server "$BUILD"/bin/llama-bench "$DEST/bin/"
# The service and model dir are agent-owned: the server runs as agent and
# the model is swapped in by hand.
sudo chown -R agent:agent "$DEST"/bin
sudo install -d -o agent -g agent "$DEST/models"
log "Installed to $DEST/bin"

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
# 4. Service (disabled; enable with: sudo systemctl enable --now jarvis-engine-igpu)
########################################
sudo install -m 644 systemd/jarvis-engine-igpu.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl disable jarvis-engine-igpu 2>/dev/null || true
log "Service installed but DISABLED. Model: put a GGUF at /opt/jarvis-igpu/models/jarvis-igpu.gguf, then:"
log "  sudo systemctl enable --now jarvis-engine-igpu"
