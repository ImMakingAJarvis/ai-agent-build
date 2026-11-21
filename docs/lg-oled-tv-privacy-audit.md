# LG OLED TV privacy audit — plan

Target: `111.1.2.167` — LG **OLED48B5PUA** (48" B5-series OLED, 2025 model,
US, webOS 25 — confirm the exact build under Settings → General → About this TV).

Goal: determine whether the TV is collecting or sending data about us
(microphone, camera, screen content, viewing behaviour) and understand
technically what it does, by reverse-engineering it. Owner's own device on
the owner's own network; every step below is non-destructive and reversible.
We do not flash custom firmware onto a webOS TV (no safe path exists, and we
don't need one).

Status: **Phase 0 done** (2026-11-20). Results below.

## Phase 0 — recon (done, 2026-11-20)

| Item | Result |
|---|---|
| Model | OLED48B5PUA — from mDNS/Chromecast device name |
| mDNS | `LGwebOSTV.localdomain`; advertises `_googlecast._tcp` (Chromecast built in) |
| MAC | `6c:15:db:53:b2:3a`, same L2 segment as this box (iface `enxf44dad08206f`) |
| TCP open | 515, 1024 (**LG WebOS upnpd**, UPnP 1.0), 3000, 3001 (ssl/http, "LG smart TV http service"), 7000 (RTSP), 8008 (http), 8009 (**Google Cast** control), 8443 (ssl/https) |
| TCP closed | 192 of top-200 scanned |
| Note | nmap's "WebOS 4.0.0" banner is the UPnP *protocol* version, not the OS version |

Still to record on the TV itself: firmware build number (decides which
firmware `.pkg` to fetch and whether the jailbreak applies).

## Phase 1 — passive observation (24–72 h, zero interaction with the TV)

Question: what does it send, to whom, and when?

1. **DNS logging (primary).** `dnsmasq` is installed on this box. Point the
   TV's DNS here via the router's DHCP reservation (reversible): dnsmasq with
   `--log-facility=/var/log/tv-dns.log`, forwarding to the normal resolver.
   Every query is logged with a timestamp → a complete timeline of the domains
   the TV talks to, including TLS SNI for the https ones.
2. **pcap (supplement).**
   `tcpdump -i enxf44dad08206f host 111.1.2.167 -w /var/log/tv/pcap-<date>.pcap`
   captures only what crosses this segment (TV↔LAN: mDNS/SSDP/cast).
   TV→internet flows through the router; capture there if a full view is
   wanted.
3. **Event correlation.** Keep a small log: TV power on/off, app opens,
   channel changes, voice-remote uses. Compare beacon timestamps against events.

What to look for (LG-specific, given the 2023 FTC settlement context):
- LG account / webOS cloud endpoints (account, push, content metadata).
- **ACR ("Always Connected Research")**: LG's ad-ID telemetry. The FTC
  settlement acknowledged ACR could include *screen-content* data on some
  models/versions. Check Settings → General → Privacy; watch for ACR-ID
  refresh calls on the network.
- Ad/ops networks (googlesyndication, doubleclick, taboola, …).
- Google Cast traffic: this TV joins a Cast group with several Nest devices.
- **Mic test**: press the voice button and say nothing → does a network event
  fire? (The B5 has no camera; the mic is in the voice remote — check whether
  a remote with a mic is paired.)
- Beacon cadence: a call every N minutes with zero use = always-on telemetry.

Output: endpoint table (domain, first seen, frequency, SNI/cert) plus a
verdict per channel: mic / screen / behaviour / ads.

## Phase 2 — in-OS audit (no exploit needed)

- Walk every privacy toggle in Settings (Privacy, ACR, LG account, data
  collection, per-app camera/mic permissions); document before/after state.
- Installed-app list — every third-party app is its own data channel.
- Built-in web browser: load a properties page (`navigator.*`, media-device
  enumeration, permission probes) and document what the browser exposes to
  web content.

## Phase 3 — webOS Developer Mode + SDK (official, sanctioned access)

1. Install the "Developer Mode" app from the LG Content Store; enable it
   (needs a free LG developer account).
2. From this box: `pip install webostv ares-cli` (webosbrew tooling).
   `webos-tv-dev-manager pair` → device token (stored locally).
3. `ares create` a probe app, run it on the TV:
   - enumerate installed apps and their data directories,
   - query the webOS API surface (what capabilities exist),
   - list network interfaces / DNS config.
4. Test the permission model: dev-mode apps can request permissions
   (network, media, …) with user approval — see what a fully-permissioned
   dev app can observe.

## Phase 4 — jailbreak / shell + static RE (if supported)

- Check the webosbrew hardware table ([webosbrew.org](https://www.webosbrew.org/))
  for OLED48B5PUA (2025 / webOS 25). The community TV jailbreak exists
  ("Jailbreak your TV and install Homebrew Channel") but 2025 models are new —
  verify support before relying on this path.
- If supported: jailbreak → root shell + Homebrew Channel → dump the rootfs,
  extract installed `.ipk` app packages, `strace` the telemetry daemons, read
  cron/startup scripts.
- Static RE either way: LG publishes firmware `.pkg` updates per model
  (or capture the OTA update in flight). Unpack, then Ghidra + strings:
  telemetry endpoints and the ACR implementation, mic/camera consumers
  (which processes open audio devices), upload code paths, certificate pins.
- "Reverse Engineering Anything" is interpreted here as the standard RE
  pipeline — recon → passive → dynamic → static → verify — applied to each
  component found. If a specific tool was meant, name it and it gets
  substituted in.

## Phase 5 — report + hardening

- Evidence-based report: verdict per channel with log excerpts.
- Hardening (per findings): disable ACR, sign out of the LG account,
  remove/stop mic-capable apps, consider a restricted-internet VLAN for the
  TV; optionally keep the DNS logging as a standing monitor (small cron,
  weekly "new domains" report).

## Rules

- All reversible; no firmware flashing; no bricking.
- OTA updates can invalidate Phase 3/4 access — re-verify after any TV update.
- Captures and logs live under `/var/log/tv/` (git-ignored); never committed
  to this repo.

## Execution checklist

- [x] Phase 0 recon: identity, mDNS, port scan (2026-11-20)
- [ ] Phase 0: firmware build number from the TV
- [ ] Phase 1: dnsmasq DNS logging for the TV (24–72 h)
- [ ] Phase 1: event correlation + mic test
- [ ] Phase 2: in-OS audit
- [ ] Phase 3: dev mode + webostv pairing + probe app
- [ ] Phase 4: jailbreak support check → shell or static firmware RE
- [ ] Phase 5: report + hardening
