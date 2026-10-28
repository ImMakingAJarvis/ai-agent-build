# Runtime for the brain engine.
#
# The llama-server binary is the host's pinned build (llama.cpp b11815,
# installed by scripts/install_engine.sh) and is bind-mounted from
# /opt/jarvis-engine — it is NOT baked into this image. It was built on this
# host against glibc 2.43 (Ubuntu 26.04), so the base must match: the
# nvidia/cuda images top out at ubuntu24.04 (glibc 2.39) and cannot run it.
#
# The CUDA 13.1 *toolkit* libs the binary links (libcudart/libcublas
# .so.13) are bind-mounted from /usr/local/cuda-13.1/lib64 by
# docker-compose.yml (they only need glibc <= 2.27, so this works); the
# driver lib (libcuda.so.1) is injected by the nvidia container runtime.
# libgomp1 (OpenMP) is the only package the base is missing.
FROM ubuntu:26.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates libgomp1 \
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/opt/jarvis-engine/bin/llama-server"]
