# CUDA runtime for the brain engine.
#
# The llama-server binary is the host's pinned build (llama.cpp b11815,
# installed by scripts/install_engine.sh) and is bind-mounted from
# /opt/jarvis-engine — it is NOT baked into this image. It links
# libcudart.so.13 / libcublas.so.13, which the CUDA 13.1 runtime image
# provides; the driver lib (libcuda.so.1) is mounted by the nvidia runtime.
FROM nvidia/cuda:13.1.1-runtime-ubuntu22.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/opt/jarvis-engine/bin/llama-server"]
