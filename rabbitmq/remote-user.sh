#!/usr/bin/env bash
# rabbitmq/remote-user.sh — provision one least-privilege RabbitMQ user for a
# remote capworker node (EVB-37, plan §3.9).
#
# Usage:
#   REMOTE_USER_PASSWORD=... rabbitmq/remote-user.sh <node> <cap1> [cap2 ...]
#
# <node> is the node's id: the user is remote-<node> and the worker's
# location/device becomes remote:<node> — it must match the node's
# capworker-remote.yaml `node:` field. The password comes from
# REMOTE_USER_PASSWORD or is generated (and printed) when unset.
# Idempotent: re-running updates the permissions; with REMOTE_USER_PASSWORD
# set it also changes the password of an existing user.
#
# The user gets exactly what the remote capworker needs, nothing more:
#   configure  tasks, events, jarvis.dlx, dead, cap.<its caps>, flow.*
#              (the worker declares these exchanges/queues at startup)
#   write      events (heartbeats), flow.* (results back to the submitter)
#   read       cap.<its caps> (its own work queues)
# Deliberately NOT granted: write on tasks (only the box publishes jobs),
# access to the dead queue (dead-lettering is broker-side), any admin tag.
#
# TLS: this script only manages the user. Off-LAN links need the broker's
# ssl listener on 5671 (rabbitmq.conf: listeners.ssl.tcp = 5671 + certs) and
# the node config switched to amqps:// with tls.ca_file pointing at the CA
# both nodes share. Inside the LAN, plain amqp:// on 5672 stays fine.

set -euo pipefail

NODE=${1:?"usage: remote-user.sh <node> <cap1> [cap2 ...]"}
shift
[ $# -ge 1 ] || { echo "at least one capability required" >&2; exit 2; }

case $NODE in
  *[!a-z0-9-]* | '') echo "node id must match [a-z0-9-]+: $NODE" >&2; exit 2 ;;
esac
for c in "$@"; do
  case $c in
    *[!a-z0-9-]* | '') echo "capability must match [a-z0-9-]+: $c" >&2; exit 2 ;;
  esac
done

USER=remote-${NODE}
VHOST=/

# rabbitmqctl: natively when installed, otherwise the jarvis-rabbitmq
# container (this box's broker).
if command -v rabbitmqctl >/dev/null 2>&1; then
  rmq() { rabbitmqctl "$@"; }
else
  rmq() { docker exec jarvis-rabbitmq rabbitmqctl "$@"; }
fi

# capability alternation: echo|transcribe
caps_alt=""
for c in "$@"; do
  [ -n "$caps_alt" && caps_alt+="|" ]
  caps_alt+=$c
done

RE_READ="^cap\.(${caps_alt})\$"
RE_WRITE="^(events|flow\..+)\$"
RE_CONFIGURE="^(tasks|events|jarvis\.dlx|dead|cap\.(${caps_alt})|flow\..+)\$"

PASSWORD=${REMOTE_USER_PASSWORD:-}
GENERATED=no
if [ -z "$PASSWORD" ]; then
  PASSWORD=$(openssl rand -hex 12)
  GENERATED=yes
fi

users_json=$(rmq list_users -p "$VHOST" --formatter json)
case $users_json in
  *'"'$USER'"'*)
    if [ -n "${REMOTE_USER_PASSWORD:-}" ]; then
      rmq change_password -p "$VHOST" "$USER" "$PASSWORD" >/dev/null
    fi
    ;;
  *)
    rmq add_user -p "$VHOST" "$USER" "$PASSWORD"
    ;;
esac
rmq set_permissions -p "$VHOST" "$USER" "$RE_READ" "$RE_WRITE" "$RE_CONFIGURE" >/dev/null

echo "Remote RabbitMQ user provisioned (vhost $VHOST):"
echo "  user      $USER"
echo "  read      $RE_READ"
echo "  write     $RE_WRITE"
echo "  configure $RE_CONFIGURE"
[ "$GENERATED" = yes ] && echo "  password  $PASSWORD  (generated)" || echo "  password  (from REMOTE_USER_PASSWORD)"
echo
echo "Node config for $NODE (capworker-remote.yaml; chmod 600 on the node):"
cat <<EOF
node: ${NODE}
broker:
  url: amqp://${USER}:${PASSWORD}@<BROKER-HOST>:5672/
  # off-LAN only: url: amqps://${USER}:${PASSWORD}@<BROKER-HOST>:5671/
  # tls:
  #   ca_file: /etc/jarvis/ca.pem
artifacts:
  dir: /mnt/nas/jarvis/artifacts
EOF
echo
echo "Remember: the password above must not be committed to the repo —"
echo "write it into the node's config file only."
