#!/usr/bin/env bash
# docker-fetch.sh — Download a file using a detached Docker container.
#
# Replaces hf-transfer for non-blocking model downloads. The container
# writes a <destination>.done flag file on success so the worker can
# poll for completion instead of blocking.
#
# Usage:
#   scripts/docker-fetch.sh <url> <destination-path> [--token <hf-token>]
#
# Examples:
#   # Download a public GGUF from HuggingFace
#   scripts/docker-fetch.sh \
#     "https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/main/Qwen3.8-27B-UD-Q4_K_M.gguf" \
#     "/opt/jarvis-engine/models/jarvis-brain.gguf"
#
#   # Download with a HuggingFace auth token
#   scripts/docker-fetch.sh \
#     "https://huggingface.co/org/model/resolve/main/file.bin" \
#     "/path/to/file.bin" \
#     --token "hf_..."
#
# The script returns immediately after launching the container. Check
# for <destination-path>.done to know when the download finished.

set -euo pipefail

usage() {
  echo "Usage: $0 <url> <destination-path> [--token <hf-token>]" >&2
  exit 1
}

[[ $# -ge 2 ]] || usage

URL="$1"
DEST="$2"
shift 2

HF_TOKEN=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token)
      [[ $# -ge 2 ]] || usage
      HF_TOKEN="$2"
      shift 2
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      ;;
  esac
done

# Ensure the destination directory exists
DEST_DIR="$(dirname "$DEST")"
mkdir -p "$DEST_DIR"

# Flag file sits next to the destination
DONE_FLAG="${DEST}.done"

# Remove any stale flag file
rm -f "$DONE_FLAG"

# Container name is derived from the destination path to allow re-launching
CONTAINER_NAME="docker-fetch-$(echo "$DEST" | sed 's|/|_|g' | cut -c1-64)"

# Stop any previous container with the same name
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# Build the Authorization header if a token was provided
AUTH_HEADER=""
if [[ -n "$HF_TOKEN" ]]; then
  AUTH_HEADER="Authorization: Bearer ${HF_TOKEN}"
fi

echo "==> Launching detached download: ${URL} -> ${DEST}"
echo "    Container: ${CONTAINER_NAME}"
echo "    Flag file: ${DONE_FLAG}"

docker run -d \
  --name "$CONTAINER_NAME" \
  --rm \
  -v "$DEST_DIR:$DEST_DIR" \
  alpine:3.20 \
  /bin/sh -c "
    set -e
    TMP='${DEST}.tmp.docker-fetch'
    DONE='${DONE_FLAG}'

    # Install wget if not present (alpine minimal may lack it)
    which wget >/dev/null 2>&1 || apk add --no-cache wget >/dev/null 2>&1

    # Download with wget (retry on failure)
    # Alpine/busybox wget: -T timeout, -O output, -q quiet, --header for auth
    RETRIES=3
    ATTEMPT=0
    while [[ \$ATTEMPT -lt \$RETRIES ]]; do
      ATTEMPT=\$((ATTEMPT + 1))
      if wget -q -T 300 \
        ${AUTH_HEADER:+--header='${AUTH_HEADER}'} \
        -O \"\$TMP\" '${URL}'; then
        mv \"\$TMP\" '${DEST}'
        echo \"\$(date -u '+%Y-%m-%dT%H:%M:%SZ') download complete: ${DEST}\" > \"\$DONE\"
        exit 0
      fi
      echo \"Attempt \$ATTEMPT/\$RETRIES failed, retrying...\" >&2
      sleep \$((ATTEMPT * 10))
    done

    # All retries exhausted
    rm -f \"\$TMP\"
    echo \"\$(date -u '+%Y-%m-%dT%H:%M:%SZ') download failed after \$RETRIES attempts\" > \"\$DONE.fail\"
    exit 1
  "

echo "==> Container launched. Check '${DONE_FLAG}' for completion."
