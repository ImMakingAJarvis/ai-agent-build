# Remote capworker — running a capability on a second machine

Plan §3.9 (EVB-37): a remote node runs the same capability workers as the
box, packaged as **one static Go binary + one YAML config**. The node
serves its configured `cap.<name>` queues with location `remote:<node>`
(consumer priority 1 — a slow link never cuts in front of the box's local
workers), heartbeats `worker.<node>.*` on the events exchange, and
self-reports its own free RAM/VRAM to resourced (`POST /device-report`) —
**read-only lease accounting**: the remote reports itself and never takes
leases from the box (resourced denies lease requests for `remote:*`
devices; its RAM is excluded from the box's pool).

## Build (on the box)

```
CGO_ENABLED=0 go build -o capworker-remote ./cmd/capworker-remote
```

The binary is static; copy it and the config to the node (e.g. over the
NAS share or scp). No other dependency — no Go toolchain, no repo.

## Provision the node's broker user (on the box)

```
REMOTE_USER_PASSWORD=$(openssl rand -hex 12) rabbitmq/remote-user.sh <node> <cap1> [cap2 ...]
```

Creates `remote-<node>` with exactly the permissions the worker declares
at startup (consume its `cap.<its caps>`; publish `flow.*` + `events`;
configure the exchanges/queues it needs) and nothing more — no `tasks`
write, no dead-queue access, no admin tag. Re-running is idempotent
(updates permissions; rotates the password when `REMOTE_USER_PASSWORD`
is set). It prints the permission regexes, the password, and a config
snippet. The password goes into the node's config file only — never into
the repo.

## Node config (`capworker-remote.yaml`, chmod 600)

```yaml
node: node2                     # must match the provisioned node id
broker:
  url: amqp://remote-node2:<password>@<BROKER-HOST>:5672/   # inside the LAN
  # off-LAN: amqps:// on 5671 with the shared CA:
  # url: amqps://remote-node2:<password>@<BROKER-HOST>:5671/
  # tls:
  #   ca_file: /etc/jarvis/ca.pem
resourced:
  url: http://<BOX-HOST>:9104   # self-report target; omit to turn it off
artifacts:
  dir: /mnt/nas/jarvis/artifacts  # the NAS share both machines mount
heartbeat: 30s
self_report: 60s
capabilities:
  - name: echo                  # built-in test capability (cmd/cap-echo contract)
    builtin: echo
    delay: 10s
  - name: transcribe            # any external program
    command: [/usr/local/bin/transcribe.sh]
    env: [API_URL=http://...]
    timeout: 10m
```

## Run

```
./capworker-remote -config /etc/jarvis/capworker-remote.yaml
```

A config error exits 2 (operator error — do not restart-loop on it).
Broker outages are retried with backoff; resourced outages are logged and
retried on the next tick (it is an observation point, not a dependency).

### Command handler protocol

`command:` runs one program per request: the request envelope (the
`queue.Message` JSON) arrives on **stdin**; the program prints one JSON
result to **stdout**:

```json
{"status": "ok", "summary": "...", "artifacts": ["$JARVIS_OUT_DIR/out.mp3"], "data": {...}}
```

`status` is `ok` (default), `error` or `timeout`; a non-zero exit is an
`error` Result whose message carries the tail of stderr; the program is
killed at `timeout`. It gets `JARVIS_CAP`, `JARVIS_CORR`, `JARVIS_OUT_DIR`
(write artifacts there — both machines see the NAS path) and
`JARVIS_REPLY_TO` in its environment, on top of the config's `env` list.

## TLS (off-LAN links)

Inside the LAN, plain `amqp://` on 5672 is fine. Off-LAN, enable the
broker's SSL listener (`listeners.ssl.tcp = 5671` + a certificate from the
CA both nodes share), point `broker.url` at `amqps://`, and set
`broker.tls.ca_file` to that CA. A bad `ca_file` fails the dial (it does
not silently fall back to the system roots). `insecure_skip_verify`
disables certificate checking — encryption only; LAN/lab debugging, never
an off-LAN link.

## Verify (on the box)

```
jarvis-mq submit <cap> --reply-to flow.<job-id> --corr <id>
jarvis-mq results flow.<job-id>              # the Result (artifacts on the NAS)
jarvis-mq tail events "worker.<node>.*"      # heartbeats + lifecycle events
curl -s localhost:9104/state | jq '.devices[] | select(.name|startswith("remote:"))'
```

The remote device appears in resourced with its self-reported RAM/VRAM;
it stays `present` while it keeps reporting (stale after
`REMOTE_REPORT_STALE`, default 3 min) and is never leaseable.
