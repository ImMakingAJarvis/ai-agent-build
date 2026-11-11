# Jarvis Chat — React Native (Expo) app

The installable native app for the local brain. It talks to the **same
backend** as the PWA (`scripts/jarvis_app/`, served at
`https://111.1.2.27:8443/app/`) — one codebase for iOS, Android and web
(`react-native-web`).

Why React Native + Expo instead of Capacitor/Flutter, and what this scaffold
covers: see the "Native app — framework decision" section in
`docs/mobile-chat-app.md` (repo root).

## Backend (unchanged by this app)

`scripts/voice_service.py` in this repo:

| Endpoint      | Used for                                   |
| ------------- | ------------------------------------------ |
| `POST /chat`  | text chat; `{messages, model?}` → `{reply, scheduled, actions}` (action chips: ⚙ queued / 🔎 tool runs) |
| `POST /voice` | mic upload (multipart `file`, optional `model`) → TTS audio + `X-Transcript` / `X-Reply` headers |
| `GET /models` | model picker (grows when more models load) |
| `GET /health` | offline/LAN banner probe                   |

Optional HTTP Basic auth (username/password in Settings) — the service's
auth is off by default, same as the PWA.

## Run

```sh
npm install

# Dev server: Expo Go on a phone (home WiFi/VPN), or web in the browser
npx expo start

# Web build (static, serve dist/ anywhere)
npx expo export --platform web

# Native JS bundles (build check without a device)
npx expo export --platform android
npx expo export --platform ios
```

## Server

Default, per platform (changeable in **⚙ Settings**):

- **iOS + web**: `https://111.1.2.27:8443` — self-signed TLS, the same
  cert the PWA and the browser voice-chat page use; the phone already
  trusts the LAN CA profile.
- **Android**: `http://111.1.2.27:8100` — the same service's plain-HTTP
  port. Android apps do not trust user-installed CAs, so the phone avoids
  certificates entirely.
- **Android emulator**: `http://10.0.2.2:8100` (host loopback alias).

## Voice input (tap-to-talk)

🎙 in the input bar records until you tap ⏹ — or **automatically**, ~5 s
after you stop talking (ported from the PWA's VAD fix, PR #79). The
recording is uploaded as multipart to `POST /voice`; the server transcodes
whatever container the phone records in (m4a/AAC, webm) via ffmpeg and
returns the transcript (`X-Transcript`), the reply (`X-Reply`) and the TTS
audio, which plays natively (speaker, even in iOS silent mode). The
transcript + reply join the chat history, so later turns keep the spoken
context.

- The VAD state machine is pure: `src/vad.js`, tested without a device by
  `node test/vad.test.js` (scripted mic levels).
- Tuning: `VAD_SILENCE_MS` (5000), `VAD_MIN_RECORD_MS` (2000),
  `VAD_THRESHOLD` (0.01 — raise for noisy rooms; this is the knob to move
  if on-device metering runs hot or quiet vs the PWA's AnalyserNode RMS),
  `MAX_RECORD_MS` (30000) — top of `src/vad.js`.
- Flow: `idle → recording → sending → playing → idle`; errors surface as
  🎙 error bubbles (e.g. "No speech detected in audio" from the server).

## Scope notes

Text chat end-to-end, model picker, voice tap-to-talk with silence
auto-stop (this PR), "🧩 Queue that as a job" (same prompt the PWA sends),
settings, offline/LAN banner, web target. Real-device verification and
phone install (Expo Go now; store/EAS build later) are follow-up job
`pr76-app-verify`.
