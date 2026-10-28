#!/usr/bin/env bash
# Install + enable the jarvis-stack boot unit. Idempotent.
#
# The unit points at the canonical compose path in the main repo tree
# (/home/agent/ai-agent-build/docker-compose.yml), which exists once the
# phase-services-containers PR is merged. Containers are independent of the
# unit: they come back on boot via their restart: unless-stopped policies.
set -euo pipefail
cd "$(dirname "$0")/.."
sudo cp systemd/jarvis-stack.service /etc/systemd/system/jarvis-stack.service
sudo systemctl daemon-reload
sudo systemctl enable jarvis-stack.service
systemctl is-enabled jarvis-stack.service
