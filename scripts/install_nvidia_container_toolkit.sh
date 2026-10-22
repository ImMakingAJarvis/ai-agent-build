#!/usr/bin/env bash
# Install and configure NVIDIA Container Toolkit so Docker containers can
# access the GPU via --gpus all.
set -euo pipefail

# Install the toolkit (from the CUDA repo already configured on this system).
if ! dpkg -s nvidia-container-toolkit >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y nvidia-container-toolkit
fi

# Configure the NVIDIA runtime in Docker's daemon.json.
# nvidia-ctk creates the file if it doesn't exist, or merges the runtime
# section into an existing config.
sudo nvidia-ctk runtime configure --config /etc/docker/daemon.json

# Restart Docker to pick up the new runtime.
sudo systemctl restart docker
sudo systemctl is-active docker

# Verify: run nvidia-smi inside a container.
echo
docker run --rm --gpus all nvidia/cuda:12.6.0-base-ubuntu24.04 nvidia-smi
echo
echo "NVIDIA Container Toolkit OK"
