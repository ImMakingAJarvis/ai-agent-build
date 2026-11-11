# Mobile chat app (Jarvis Chat PWA)

Job `chat-1793077879-058a`. A voice + text chat app for iPhone and Android to
talk, chat, and plan with the local brain — reachable only on the home network
or the VPN, by construction (the LAN IP is not routable elsewhere).

## Why a PWA, not a native app

- "Works only on my LAN/VPN" rules out App Store / Play distribution: an
  installable, self-contained web app on `https://111.1.2.27:8443/app/` is
  the only sane delivery path.
- One codebase covers iPhone and Android; the mic already needs HTTPS, which
  the voice service provides (self-signed cert for the LAN IP + localhost).
- The box is Linux, so an iOS build would need a Mac anyway.
- iOS: Safari → Share → **Add to Home Screen** (full-screen, own icon).
  Android: Chrome **Install app** prompt (or menu → Install app).

## What was built

- **`scripts/jarvis_app/`** — the app: `index.html`, `style.css`, `app.js`,
  `sw.js` (service worker: caches the shell, never intercepts API calls),
  `manifest.webmanifest`, `icons/` (generated, see below). Dark theme,
  safe-area aware, chat bubbles, mic button with recording state, model
  picker in the header, "Queue that as a job" button, login overlay (only
  appears when service auth is enabled), offline banner.
- **`scripts/gen_app_icons.py`** — PIL generator for the arc-reactor icon
  (192/512/apple-touch). Re-run and commit the PNGs if the art changes.
- **`scripts/voice_service.py`** (additive; existing endpoints unchanged):
  - `POST /chat` — stateless text chat. Client sends the message history
    (`{"messages":[{role,content}...], "model"?}`); server prepends the
    persona prompt and runs the same tool loop as voice (`schedule_job`,
    `search_jobs`), with the same "I queued it" guardrail. Returns
    `{"reply", "scheduled", "actions"}` where `actions` lists every tool run
    so the app can show "⚙ Queued: …" / "🔎 …" chips.
  - `GET /models` — lists what the brain engine serves today (queries
    `/v1/models`; falls back to the configured default). This is the
    model picker's data source — it grows automatically if more models are
    loaded (see below).
  - `POST /voice`, `POST /voice/json` — optional `model` field now overrides
    the brain for that turn. No `model` field = exactly the old behavior, so
    the Home Assistant integration is untouched.
  - `GET /app/` — serves the PWA (StaticFiles mount; assets bypass the
    Basic-auth dependency, the API does not).
  - Chat prompt (`build_chat_prompt`): same persona as the voice profile,
    tuned for text — may be a bit longer, light markdown, drives planning
    and the two tools.

## Models / the picker

Today the engine (llama-server `jarvis-engine`, :11434) serves exactly one
model: **`jarvis-brain` = Qwen 3.8 27B Q4_K_M**. The picker lists it; there
is no 32B loaded and the 24 GB card holds one ~20 GB model at a time — a
bigger/different brain is a model-swap job, not an app change. If Nathan
swaps or adds models, `/models` and the app's dropdown pick them up with no
app update.

## Installing on a phone

1. Be on the home WiFi or connected to the VPN.
2. Open `https://111.1.2.27:8443/app/` in Safari (iOS) or Chrome (Android).
3. Accept the self-signed certificate warning (it is the same cert the
   browser voice-chat page already uses).
4. iOS: Share → **Add to Home Screen**. Android: tap the **Install app**
   button (or menu → Install app).
5. Grant the mic permission when asked. Talk (🎙 hold-free tap-to-talk,
   tap again to stop), type, or hit 🧩 to turn the last reply into a queued
   job.

No account needed: service auth is currently off (the
`/opt/jarvis-voice/auth-off.env` drop-in, because the HA integration does
not send credentials). If auth is re-enabled, the app shows its own login
overlay and sends Basic auth with API calls.

## Deploy

The app ships inside the voice service deployment:

```
cp -r scripts/jarvis_app /opt/jarvis-voice/scripts/
# voice_service.py also copied; /opt/jarvis-voice/DEPLOYED_FROM = commit hash
cd <repo> && docker compose up -d voice
```

The container must publish `8443` and bind-mount the TLS cert
(`/home/agent/.config/jarvis/tls`, see `docker-compose.yml`) or the mic
will still be blocked — the browser only allows it in a secure context.
Regenerate the cert with `scripts/gen_voice_cert.sh` after the IP changes.

Verify: `curl -sk https://localhost:8443/app/` → 200 HTML, and
`curl -s http://localhost:8100/health` → ok.

## Test results (2026-10-27, throwaway instance on :18100)

- `/health`, `/models` (clean name `jarvis-brain`), `/app/` (307 on `/app`),
  manifest / sw.js / app.js / icons all 200 with correct content types.
- `POST /chat` plain question → reply, no actions. `POST /chat` with a
  history question → `search_jobs` ran and was reported in `actions`.
- `POST /voice` with an espeak-ng wav, **no** model field (the HA call
  shape) → 200, perfect transcript, persona-correct reply, TTS wav,
  ~10 s end-to-end. With `model=jarvis-brain` field → 200, ~7 s.

## Native app (React Native + Expo)

Framework decision: **React Native + Expo** (SDK 58) in `jarvis-mobile/`,
one codebase for iOS, Android and web (`react-native-web`). Capacitor would
have just re-wrapped the PWA (same webview mic caveats, two codebases);
Flutter is a bigger toolchain for a UI this small. Expo Go means a phone
can test the app by scanning a QR code — no store, no signing — which is
what the verification round needs. The app reuses the **same** backend
(`scripts/voice_service.py`), unchanged: `/chat`, `/voice`, `/models`,
`/health` (plus optional Basic auth, off by default).

Voice input (PR: real app native mic + 5 s silence auto-stop):

- **Tap-to-talk** mic button in the chat bar (🎙 → red ⏹ with a live
  second count; ⏹ still stops and sends immediately — tap-to-stop is
  unchanged from the PWA).
- **Auto-stop** is the PWA's VAD logic (PR #79, ported from commit c0175fb
  into `jarvis-mobile/src/vad.js`, pure and unit-tested in
  `test/vad.test.js`): speech-level samples reset a 5 s silence countdown,
  min record 2 s, 30 s safety cap. The level source differs — the PWA
  watched an AnalyserNode RMS, the app polls
  `recorder.getStatus().metering` (expo-audio, 100 ms ticks). Tuning
  constants live at the top of `src/vad.js` (`VAD_THRESHOLD` is the one to
  move if metering's scale differs from AnalyserNode RMS on device).
- **Upload format**: the HIGH_QUALITY preset records m4a/AAC (iOS,
  Android) and webm (web). The server transcodes any container via
  ffmpeg (`ensure_wav()`, PR #78), so no format negotiation is needed.
- **TTS playback**: the reply audio (edge-tts MP3) plays natively —
  written to the app cache and played with expo-audio on the phones,
  blob URL on web. `setAudioModeAsync({ playsInSilentMode,
  allowsRecording })` keeps TTS on the speaker on iOS even while the mic
  mode is armed.
- **Self-signed cert, per platform**: iOS and web hit
  `https://111.1.2.27:8443` (the phone already trusts the LAN CA profile —
  that is how the PWA works, and iOS trusts user CAs in apps); Android does
  **not** trust user-installed CAs, so it defaults to the same service's
  plain-HTTP port `http://111.1.2.27:8100` — no certificate involved. Both
  defaults are changeable in ⚙ Settings.
- Voice turns join the chat history: the transcript lands as the user
  bubble, the spoken reply as the assistant bubble, so later `/chat`
  turns keep the spoken context.

Status: VAD logic unit-tested (8/8, `node test/vad.test.js`), web +
android + ios JS bundles export cleanly. **Real-device verification
pending** — Expo Go on an Android phone first, then iOS (job
`pr76-app-verify`): speak a phrase → expect auto-stop ~5 s after the last
word → transcript + reply bubble + TTS audio; a <5 s pause mid-sentence
must not cut; ⏹ still stops early.
