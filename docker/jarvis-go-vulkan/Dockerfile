# Runtime for the Vulkan-capable Go workers (plan task EVB-24,
# cap.bench.run). Same build stage as docker/jarvis-go (one static Go
# binary, chosen by ARG BIN), but the base is ubuntu:26.04 rather than
# debian:trixie-slim: the mounted llama.cpp tools (llama-bench from
# /opt/jarvis-igpu) were built on this host against glibc 2.43 (Ubuntu
# 26.04), and the iGPU path needs the same loader + radeon ICD as
# docker/engine-igpu. The 4090 stays out of reach: no NVIDIA ICD is
# installed, so Vulkan0 in the container is the iGPU.
#
# python3 runs the live bench script (scripts/benchmark_q6.py, baked in
# below so the worker and the script ship together).
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./${BIN}"

FROM ubuntu:26.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        libgomp1 \
        libvulkan1 \
        mesa-vulkan-drivers \
        python3 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/app /usr/local/bin/app
COPY scripts/benchmark_q6.py /usr/local/lib/jarvis-bench/benchmark_q6.py
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/app"]
