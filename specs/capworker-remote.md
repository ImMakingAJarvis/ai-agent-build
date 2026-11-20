---
id: capworker-remote
name: Remote capability worker
summary: Runs a capability worker on another machine against this box's broker with a least-privilege user.
group: capability
kind: cli
status: optional
binaries: [cmd/capworker-remote]
ports: ["— (outbound AMQP)"]
docs: [docs/remote-capworker.md]
depends_on:
  - to: rabbitmq
    why: "consumes its cap.* queue as a remote user"
---

## Purpose

EVB-37. Lets another computer serve a capability (location
`remote:<host>`). The broker user is created by
`rabbitmq/remote-user.sh` with only the grants the reply path needs.

## Interfaces

- Consumes the configured `cap.*` queue; publishes Results to the
  request's `flow.*` queue (it may declare it).

## Configuration

- A YAML config with the broker URL, user and capability names (see
  docs/remote-capworker.md). Never commit it — it holds the password.

## Data

- None on this box.

## Deploy

Built and copied to the remote machine per docs/remote-capworker.md.

## Verify live

From the remote machine with the worker running:

    jarvis-mq call <cap> --input msg=ok      # on this box

## Failure modes

- Results nacked until the user had write on `amq.default` and read on
  `flow.*` (2026-11-15).
