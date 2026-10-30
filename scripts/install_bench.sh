#!/usr/bin/env bash
# Deploy the bench service code to the pinned deploy point /opt/jarvis-bench
# and create its data dirs. No model download here: candidate weights are
# fetched by the "Download candidate models + smoke test" job of the t2i
# bench plan (plan-jarvis-model-downloader-1793259146).
#
#   scripts/install_bench.sh   # deploy code + dirs (idempotent)
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY=/opt/jarvis-bench
LIB=/var/lib/jarvis/bench
DATA="$HOME/.local/share/jarvis/bench"

# Data dirs, agent-owned (the compose service runs as uid 1001 = agent).
sudo mkdir -p "$LIB/imgs" "$LIB/models"
sudo chown -R agent:agent "$LIB"
mkdir -p "$DATA"

if [[ ! -d "$DEPLOY" ]]; then
  sudo mkdir -p "$DEPLOY" && sudo chown agent:agent "$DEPLOY"
fi
for f in app.py config.py schema.sql models.yaml prompts.yaml test_loader.py; do
  install -m 644 "$REPO/bench/$f" "$DEPLOY/$f"
done
echo "DEPLOYED_FROM $REPO @ $(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo nogit) $(date -Is)" \
  >"$DEPLOY/DEPLOYED_FROM"
echo "==> Deployed bench service to $DEPLOY"
echo "==> Kill switch: touch /var/lib/jarvis/bench.disabled (service idles until rm)"
echo "==> Start the service: docker compose up -d bench"
