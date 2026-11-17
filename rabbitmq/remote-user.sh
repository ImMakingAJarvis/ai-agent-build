#!/usr/bin/env bash
# rabbitmq/remote-user.sh — provision one least-privilege RabbitMQ user for a
# remote capworker node (EVB-37, plan §3.9).
#
# Usage:
#   REMOTE_USER_PASSWORD=... rabbitmq/remote-user.sh <node> <cap1> [cap2 ...]
#
# <cap> is a capability name: a single word (echo) or a dotted path
# (image.generate) — the same names every submitter validates.
#
# <node> is the node's id: the user is remote-<node> and the worker's
# location/device becomes remote:<node> — it must match the node's
# capworker-remote.yaml `node:` field. The password comes from
# REMOTE_USER_PASSWORD or is generated (and printed) when unset.
# Idempotent: re-running updates the permissions; with REMOTE_USER_PASSWORD
# set it also changes the password of an existing user.
#
# The user gets what the remote capworker's declaration and publish
# paths require, and nothing beyond it:
#   configure  tasks, events, jarvis.dlx, dead, cap.<its caps>, flow.*
#              (the worker re-declares the shared topology at every
#              start — the exchanges, the dead queue, its own cap queue
#              — and re-declares the reply queue before publishing a
#              Result)
#   write      amq.default — the Result reply publishes through the
#              default exchange (routing key = the flow queue's name),
#              and basic.publish on it checks write on the default
#              exchange itself, not just on the queue;
#              tasks — the retry republish rides it (routing key
#              <capqueue>.default), so this also reaches any other
#              routing key on it: the remote node is trusted
#              infrastructure, not an untrusted client, and it is the
#              price of the shared publish path;
#              events (heartbeats); dead (the shared
#              declaration re-binds dead -> jarvis.dlx on every worker
#              start, and RabbitMQ counts a queue-to-exchange bind as a
#              write on the queue — the worker still cannot read or
#              consume dead, only re-declare the binding);
#              jarvis.dlx (declaring a cap queue with
#              x-dead-letter-exchange needs write on the DLX);
#              flow.* (the reply path re-declares and re-binds the
#              reply queue before publishing the Result);
#              cap.<its caps> (binding its own queues to tasks)
#   read       cap.<its caps> (its own work queues), flow.* (declaring
#              a queue that does not exist yet — the reply path
#              re-declares a missing reply queue so an early Result is
#              held on the broker — checks read on the queue, and the
#              consequence is that it can consume flow queues too:
#              trusted infrastructure), tasks and jarvis.dlx (binding a
#              queue to an exchange needs read on the exchange)
# Deliberately NOT granted: read/consume on dead (the dead-letter
# evidence stays on the box), any admin tag.
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
    # a dotted path of lowercase alphanumeric segments (echo, image.generate)
    *[!a-z0-9.-]* | '' | .* | *. | *..*)
      echo "capability must be a dotted path of lowercase alphanumeric segments (echo, image.generate): $c" >&2
      exit 2
      ;;
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

# capability alternation: echo|transcribe (dots escaped — they would
# otherwise be regex wildcards in the permission patterns)
caps_alt=""
for c in "$@"; do
  if [ -n "$caps_alt" ]; then
    caps_alt+="|"
  fi
  caps_alt+=${c//./\\.}
done

RE_CONFIGURE="^(tasks|events|jarvis\.dlx|dead|flow\..+|cap\.(${caps_alt}))\$"
RE_WRITE="^(amq\.default|tasks|events|dead|jarvis\.dlx|flow\..+|cap\.(${caps_alt}))\$"
RE_READ="^(cap\.(${caps_alt})|flow\..+|tasks|jarvis\.dlx)\$"

PASSWORD=${REMOTE_USER_PASSWORD:-}
GENERATED=no
if [ -z "$PASSWORD" ]; then
  PASSWORD=$(openssl rand -hex 12)
  GENERATED=yes
fi

users_json=$(rmq list_users -p "$VHOST" --formatter json)
EXISTS=no
case $users_json in
  *'"'$USER'"'*)
    EXISTS=yes
    if [ -n "${REMOTE_USER_PASSWORD:-}" ]; then
      rmq change_password -p "$VHOST" "$USER" "$PASSWORD" >/dev/null
    fi
    ;;
  *)
    rmq add_user -p "$VHOST" "$USER" "$PASSWORD"
    ;;
esac
# rabbitmqctl set_permissions takes CONF, WRITE, READ (in that order).
rmq set_permissions -p "$VHOST" "$USER" "$RE_CONFIGURE" "$RE_WRITE" "$RE_READ" >/dev/null

# What the node should actually use: on a re-run without
# REMOTE_USER_PASSWORD the password is unchanged, so show a placeholder
# instead of a fresh random one that was never applied.
if [ "$EXISTS" = yes ] && [ -z "${REMOTE_USER_PASSWORD:-}" ]; then
  SHOWN_PASSWORD='<unchanged>'
  PW_LINE="  password  (existing user, unchanged — set REMOTE_USER_PASSWORD to rotate)"
elif [ "$GENERATED" = yes ]; then
  SHOWN_PASSWORD=$PASSWORD
  PW_LINE="  password  $PASSWORD  (generated)"
else
  SHOWN_PASSWORD=$PASSWORD
  PW_LINE="  password  (from REMOTE_USER_PASSWORD)"
fi

echo "Remote RabbitMQ user provisioned (vhost $VHOST):"
echo "  user      $USER"
echo "  configure $RE_CONFIGURE"
echo "  write     $RE_WRITE"
echo "  read      $RE_READ"
echo "$PW_LINE"
echo
echo "Node config for $NODE (capworker-remote.yaml; chmod 600 on the node):"
cat <<EOF
node: ${NODE}
broker:
  url: amqp://${USER}:${SHOWN_PASSWORD}@<BROKER-HOST>:5672/
  # off-LAN only: url: amqps://${USER}:${SHOWN_PASSWORD}@<BROKER-HOST>:5671/
  # tls:
  #   ca_file: /etc/jarvis/ca.pem
artifacts:
  dir: /mnt/nas/jarvis/artifacts
EOF
echo
echo "Remember: the password above must not be committed to the repo —"
echo "write it into the node's config file only."
