#!/usr/bin/env python3
"""Dev proxy for the RN web build: forwards the brain API and adds CORS.

The native app has no CORS restriction; only the web build served from a
different origin than the voice service needs this (or a same-origin mount,
or CORS headers on the service itself — see docs/mobile-chat-app.md).

Usage:  python3 dev-proxy.py [port]     (default 8600, LAN-only box)
Then point the app's Settings at http://<this-box>:<port>.
"""
import ssl
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UP = "https://111.1.2.27:8443"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8600

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE  # self-signed LAN cert


class Handler(BaseHTTPRequestHandler):
    def _cors(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")

    def do_OPTIONS(self):
        self.send_response(204)
        self._cors()
        self.end_headers()

    def _proxy(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        headers = {}
        if self.headers.get("Authorization"):
            headers["Authorization"] = self.headers["Authorization"]
        if body:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(
            UP + self.path, data=body, method=self.command, headers=headers
        )
        try:
            with urllib.request.urlopen(req, context=CTX, timeout=600) as r:
                self.send_response(r.status)
                self._cors()
                self.send_header(
                    "Content-Type", r.headers.get("Content-Type", "application/json")
                )
                self.end_headers()
                self.wfile.write(r.read())
        except urllib.error.HTTPError as e:
            self.send_response(e.code)
            self._cors()
            self.end_headers()
            self.wfile.write(e.read())

    def do_GET(self):
        self._proxy()

    def do_POST(self):
        self._proxy()

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))


if __name__ == "__main__":
    print(f"proxying {UP} on 127.0.0.1:{PORT} with permissive CORS", file=sys.stderr)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
