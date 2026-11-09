# Runtime for the iGPU helper engine (the small always-on model).
#
# The llama-server binary is the host's pinned Vulkan build (llama.cpp,
# installed by scripts/install_igpu_engine.sh) and is bind-mounted from
# /opt/jarvis-igpu — it is NOT baked into this image (same pattern as
# docker/engine/Dockerfile for the brain). It was built on this host
# against glibc 2.43 (Ubuntu 26.04), so the base must match.
#
# Vulkan on the AMD iGPU (PCI 0000:0c:00.0, /dev/dri/card2 + renderD129):
#   libvulkan1           the loader the binary links (libvulkan.so.1)
#   mesa-vulkan-drivers  the radeon ICD (libvulkan_radeon.so) that actually
#                        drives the iGPU
# docker-compose.yml mounts /dev/dri, and this container carries no NVIDIA
# Vulkan ICD, so the iGPU is the only Vulkan device it sees — the 4090
# stays out of reach (the helper can never load the brain's GPU).
FROM ubuntu:26.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        libgomp1 \
        libvulkan1 \
        mesa-vulkan-drivers \
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/opt/jarvis-igpu/bin/llama-server"]
