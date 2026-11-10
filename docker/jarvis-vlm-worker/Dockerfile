# image-describe-worker runtime (plan task EVB-21, cap.image.describe).
#
# The worker binary is a static Go build (like docker/jarvis-go), but the
# runtime base is the brain engine image (docker/engine/Dockerfile) rather
# than debian:trixie-slim: the worker spawns the host's llama.cpp
# llama-server as a coprocess, and that binary was built against glibc 2.43
# (Ubuntu 26.04) — the engine image is exactly that runtime, plus libgomp1.
# The llama-server binary, the model GGUFs and the CUDA 13.1 toolkit libs
# are bind-mounted by compose (image-describe-worker-*), mirroring the
# engine service.
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./${BIN}"

FROM jarvis/engine:local
COPY --from=build /out/app /usr/local/bin/app
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/app"]
