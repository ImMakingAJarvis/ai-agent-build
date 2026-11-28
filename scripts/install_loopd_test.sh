#!/usr/bin/env bash
# install_loopd_test.sh — the deploy guard of scripts/install_loopd.sh,
# run against a stub curl and a stub docker (no real daemon, no real
# compose): a running op refuses the restart, --force goes ahead, and an
# unreachable daemon proceeds.
# Run: bash scripts/install_loopd_test.sh   (exit 0 = all pass)
set -u
here="$(cd "$(dirname "$0")" && pwd)"
install="$here/install_loopd.sh"
fail=0

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="$TMP/bin"
mkdir -p "$BIN"

# Stub curl: logs the call; answers /v1/state with the file in STATE_FILE
# (exit 7, like a refused connection, when STATE_FILE=none) and answers
# /healthz.
cat > "$BIN/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "${CURL_LOG:?}"
for a in "$@"; do
  case "$a" in
    *"/v1/state"*)
      [[ "${STATE_FILE}" == "none" ]] && exit 7
      cat "$STATE_FILE"; exit 0;;
    *"/healthz"*) echo ok; exit 0;;
  esac
done
exit 7
EOF
chmod +x "$BIN/curl"

# Stub docker: logs the call and does nothing.
cat > "$BIN/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "${DOCKER_LOG:?}"
exit 0
EOF
chmod +x "$BIN/docker"

TOKEN="$TMP/token"
printf 'test-token\n' > "$TOKEN"
STATE_IDLE="$TMP/state-idle.json"
printf '{"draining":false,"running_ops":[],"jobs":{}}' > "$STATE_IDLE"
STATE_BUSY="$TMP/state-busy.json"
printf '{"draining":true,"running_ops":[{"job":"task-a","op":"accept-1"}]}' > "$STATE_BUSY"

# run_install STATE_FILE_OR_none [--force]: runs the install script with
# the stubs on PATH and captures its exit code in RC and output in
# "$TMP/out.log".
run_install() {
  local state="$1"; shift
  rm -f "$TMP/curl.log" "$TMP/docker.log"
  PATH="$BIN:$PATH" CURL_LOG="$TMP/curl.log" DOCKER_LOG="$TMP/docker.log" \
    STATE_FILE="$state" LOOPD_API_URL="http://stub" LOOPD_TOKEN_FILE="$TOKEN" \
    bash "$install" "$@" >"$TMP/out.log" 2>&1
  RC=$?
}

# 1. A running op refuses the restart (exit 1, no docker call, the op
#    named).
run_install "$STATE_BUSY"
if [[ $RC -ne 1 ]]; then
  echo "FAIL: busy daemon must refuse the restart (rc=$RC)"; fail=1
else
  [[ -f "$TMP/docker.log" ]] && { echo "FAIL: docker was called while an op was running"; fail=1; }
  grep -q "task-a/accept-1" "$TMP/out.log" || { echo "FAIL: the running op is not named:"; cat "$TMP/out.log"; fail=1; }
fi

# 2. --force goes ahead (exit 0, the restart runs).
run_install "$STATE_BUSY" --force
if [[ $RC -ne 0 ]]; then
  echo "FAIL: --force must go ahead (rc=$RC):"; cat "$TMP/out.log"; fail=1
else
  grep -q "up -d loopd" "$TMP/docker.log" || { echo "FAIL: --force did not reach the restart"; fail=1; }
fi

# 3. An unreachable daemon proceeds (exit 0, the restart runs).
run_install none
if [[ $RC -ne 0 ]]; then
  echo "FAIL: an unreachable daemon must not block the install (rc=$RC):"; cat "$TMP/out.log"; fail=1
else
  grep -q "up -d loopd" "$TMP/docker.log" || { echo "FAIL: unreachable case did not reach the restart"; fail=1; }
fi

# 4. An idle daemon proceeds (exit 0, the restart runs).
run_install "$STATE_IDLE"
if [[ $RC -ne 0 ]]; then
  echo "FAIL: an idle daemon must not block the install (rc=$RC):"; cat "$TMP/out.log"; fail=1
else
  grep -q "up -d loopd" "$TMP/docker.log" || { echo "FAIL: idle case did not reach the restart"; fail=1; }
fi

if [[ $fail -eq 0 ]]; then
  echo "ok: install_loopd_test"
  exit 0
fi
echo "install_loopd_test: FAILURES above"
exit 1
