# Builds one Jarvis Go service; the service package is chosen by ARG BIN
# (e.g. cmd/jarvis-chat). Context is the repo root.
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./${BIN}"

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/app /usr/local/bin/app
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/app"]
