#!/usr/bin/env bash
# igpu-bench: benchmark the Ryzen iGPU (Vulkan) as a model handler.
#
# Compares prompt-processing and token-generation speed of a model on:
#   iGPU  (Radeon graphics in the Ryzen 7 7800X3D, RADV Vulkan)
#   CPU   (no offload)
#   GTX   (RTX 4090 via the same Vulkan build, for reference)
#
# Usage:
#   scripts/igpu-bench.sh [path/to/model.gguf]...
#
# With no arguments it uses the local Ollama models llama3.2:1b and
# qwen2.5:7b (their GGUF blobs are copied from the Ollama store, no
# download needed).
#
# Prereqs: a Vulkan-enabled llama.cpp build. If
# /opt/jarvis-igpu/bin/llama-bench does not exist, run
#   scripts/install_igpu_engine.sh --build-only
# first. The calling user needs to be in the video and render groups
# (install_igpu_engine.sh does this).
set -uo pipefail

BIN="${BIN:-/opt/jarvis-igpu/bin/llama-bench}"
OLLAMA_BLOBS=/usr/share/ollama/.ollama/models/blobs
OLLAMA_MANIFESTS=/usr/share/ollama/.ollama/models/manifests/registry.ollama.ai/library
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[[ -x "$BIN" ]] || { echo "llama-bench not found at $BIN (run scripts/install_igpu_engine.sh --build-only)" >&2; exit 1; }

# model path may be a file path or an ollama name like llama3.2:1b
resolve_model() {
  local name="$1"
  if [[ -f "$name" ]]; then cp "$name" "$WORK/"; echo "$WORK/$(basename "$name")"; return; fi
  local manifest="$OLLAMA_MANIFESTS/${name%%:*}/${name##*:}"
  [[ -f "$manifest" ]] || { echo "model '$name': no file and no local Ollama manifest at $manifest" >&2; return 1; }
  local digest
  digest=$(python3 -c "
import json
d = json.load(open('$manifest'))
print(max(d['layers'], key=lambda l: l['size'])['digest'].replace('sha256:', 'sha256-'))")
  [[ -f "$OLLAMA_BLOBS/$digest" ]] || { echo "model '$name': blob $digest not in Ollama store" >&2; return 1; }
  cp "$OLLAMA_BLOBS/$digest" "$WORK/$(echo "$name" | tr ':' '-').gguf"
  echo "$WORK/$(echo "$name" | tr ':' '-').gguf"
}

# Device names: order comes from '$BIN --list-devices' (Vulkan0 = 4090,
# Vulkan1 = iGPU on this machine).
AMD_DEV="${AMD_DEV:-Vulkan1}"
NVIDIA_DEV="${NVIDIA_DEV:-Vulkan0}"

[[ $# -gt 0 ]] || set -- llama3.2:1b qwen2.5:7b
models=()
for m in "$@"; do
  path=$(resolve_model "$m") || exit 1
  models+=("$path")
done

# GPU runs must not overlap CPU runs: the iGPU reads weights through
# system RAM, so concurrent CPU inference would distort its numbers.
for mode in "$AMD_DEV" "none" "$NVIDIA_DEV"; do
  for m in "${models[@]}"; do
    devflag=$([[ "$mode" == "none" ]] && echo "-ngl 0" || echo "-dev $mode -ngl 99")
    echo "=== $(basename "$m") [$mode]"
    "$BIN" -m "$m" $devflag -p 512 -n 512 -r 3 2>&1 | grep -E "model +:|pp512|tg512" | sed 's/^/  /'
  done
done
