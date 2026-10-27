#!/usr/bin/env bash
# gen_voice_cert.sh — self-signed TLS cert for the voice service's browser UI.
#
# Browsers only expose the microphone (navigator.mediaDevices.getUserMedia)
# in secure contexts — https:// or localhost — so the /voice/chat page needs
# HTTPS when opened from another machine on the LAN. This writes a 10-year
# self-signed cert covering localhost, this box's hostname and its current
# IPv4 addresses into ~/.config/jarvis/tls/ (gitignored, outside the repo).
#
# voice_service.py picks it up automatically on (re)start.
# Re-run after the machine's IP changes (the old SAN list goes stale), then:
#   sudo systemctl restart jarvis-voice
set -euo pipefail

TLS_DIR="${TLS_DIR:-$HOME/.config/jarvis/tls}"
DAYS="${DAYS:-3650}"
host_short="$(hostname)"

ips=()
for ip in $(hostname -I 2>/dev/null); do
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && ips+=("$ip")
done

san="DNS:$host_short,DNS:${host_short}.local,DNS:localhost,IP:127.0.0.1"
for ip in "${ips[@]:-}"; do
  [[ -n "$ip" ]] && san+=",IP:$ip"
done

mkdir -p "$TLS_DIR"
chmod 700 "$TLS_DIR"

# Regenerate in place; the service must be restarted afterwards.
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$TLS_DIR/voice.key" \
  -out "$TLS_DIR/voice.crt" \
  -days "$DAYS" \
  -subj "/CN=${host_short}.local" \
  -addext "subjectAltName=${san}"

chmod 600 "$TLS_DIR/voice.key"
chmod 644 "$TLS_DIR/voice.crt"

echo "Wrote $TLS_DIR/voice.crt + voice.key"
echo "SAN: $san"
echo "Restart to apply: sudo systemctl restart jarvis-voice"
