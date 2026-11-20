# Builds one Jarvis review service (Job 12a of
# docs/pr-review-build-plan.md); the service package is chosen by ARG BIN
# (cmd/review-worker for the three model reviewers, cmd/review-sweep for the
# five-minute check). It is the jarvis-go shape plus the host tools a
# reviewer needs: git (worktrees, commits, push), gh (the merge gate), and
# the claude CLI (the reviewer itself).
#
# CPU-only on purpose: the claude CLI talks to the Anthropic API, so these
# images must never get an nvidia runtime, device requests, or
# NVIDIA_VISIBLE_DEVICES — TestComposeNoGPU in cmd/review-worker fails if a
# review service block in docker-compose.yml gains one.
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./${BIN}"

FROM debian:trixie-slim
# ca-certificates/curl/openssl: gh + the ghapp-token credential helper (a
# bash script that signs a JWT with the app key); git: worktrees and push;
# nodejs/npm: the claude CLI.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git openssl \
    nodejs npm \
 && rm -rf /var/lib/apt/lists/*
# gh: pinned release tarball, same version as the host.
ARG GH_VERSION=2.46.0
RUN curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
    | tar xz -C /usr/local --strip-components=1 "gh_${GH_VERSION}_linux_amd64/bin/gh"
# The claude CLI the reviewer execs (cmd/review-worker runs `claude`): pinned
# npm global install, same version as the host.
RUN npm install -g @anthropic-ai/claude-code@2.1.306
COPY --from=build /out/app /usr/local/bin/app
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/app"]
