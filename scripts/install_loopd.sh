#!/usr/bin/env bash
# install_loopd.sh — build the loopd image and (re)start the compose
# service (the spec's Deploy step, card ELR-7).
#
# Deploy guard (card ELR-99, M4.1g): before the restart the script asks
# the running daemon for its state (GET /v1/state, token from the token
# file). A deploy never restarts loopd over a running long op: when
# running_ops is not empty it exits 1 with the list unless --force is
# given. When loopd is not reachable — or the token file is missing, so
# the daemon cannot be up — the install proceeds.
#
# Usage: scripts/install_loopd.sh [--force]
#   --force  restart anyway, naming the running ops
# Env:
#   LOOPD_API_URL    the daemon's API address (default http://127.0.0.1:9110)
#   LOOPD_TOKEN_FILE the bearer token file (default /run/loopd/token, the
#                    loopd.token_file default in the runtime config)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ADDR="${LOOPD_API_URL:-http://127.0.0.1:9110}"
TOKEN_FILE="${LOOPD_TOKEN_FILE:-/run/loopd/token}"
FORCE=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    *) echo "usage: install_loopd.sh [--force]" >&2; exit 2 ;;
  esac
done

cd "$REPO_ROOT"

echo "==> Checking the running daemon for ops in flight (deploy guard)"
if [[ ! -r "$TOKEN_FILE" ]]; then
  # The daemon fails to start without a usable token file, so a missing
  # token file means loopd cannot be running.
  echo "==> no token file at $TOKEN_FILE; loopd cannot be running; proceeding"
else
  TOKEN="$(cat "$TOKEN_FILE")"
  STATE="$(curl -fsS --max-time 5 -H "Authorization: Bearer $TOKEN" \
    "$ADDR/v1/state" 2>/dev/null || true)"
  if [[ -z "$STATE" ]]; then
    echo "==> loopd not reachable at $ADDR; proceeding"
  else
    if ! OPS="$(printf '%s' "$STATE" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for o in d.get("running_ops") or []:
    print(o.get("job", "?") + "/" + o.get("op", "?"))
' 2>/dev/null)"; then
      echo "ERROR: could not parse the /v1/state answer:" >&2
      printf '%s\n' "$STATE" >&2
      exit 1
    fi
    if [[ -n "$OPS" && $FORCE -eq 0 ]]; then
      {
        echo "ERROR: loopd has running ops; refusing to restart over them:"
        printf '%s\n' "$OPS"
        echo "Drain first (loopctl drain --wait 10m) or pass --force."
      } >&2
      exit 1
    fi
    if [[ -n "$OPS" ]]; then
      echo "==> --force: restarting anyway; running ops:"
      printf '%s\n' "$OPS"
    else
      echo "==> no running ops"
    fi
  fi
fi

echo "==> Building the loopd image"
docker compose --profile loop build loopd

echo "==> Restarting loopd"
docker compose --profile loop up -d loopd

echo "==> Waiting for /healthz"
for i in $(seq 1 10); do
  if curl -fsS --max-time 2 "$ADDR/healthz" >/dev/null 2>&1; then
    echo "==> loopd is up"
    exit 0
  fi
  sleep 1
done
echo "==> ERROR: loopd did not answer /healthz at $ADDR" >&2
exit 1
