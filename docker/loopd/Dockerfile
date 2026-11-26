# Builds the event-loop runtime (card ELR-7). The build stage is the shared
# Go build from docker/jarvis-go/Dockerfile, extended to build both
# cmd/loopd and cmd/loopctl. The final stage stays on the Go image instead
# of debian:trixie-slim and adds git and the gh CLI: later cards run builds
# and tests inside this container.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loopd ./cmd/loopd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loopctl ./cmd/loopctl

FROM golang:1.26
# git and gh: the runtime runs git, builds and tests inside the container.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git gh \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/loopd /usr/local/bin/loopd
COPY --from=build /out/loopctl /usr/local/bin/loopctl
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["loopd"]
