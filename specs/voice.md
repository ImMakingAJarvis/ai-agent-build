---
id: voice
name: Voice service (jarvis-voice)
summary: STT → brain → TTS pipeline, the browser voice chat and the Jarvis Chat PWA, the HA voice integration endpoint.
group: face
kind: container
status: active
compose: [voice]
binaries: [scripts/voice_service.py, scripts/gen_voice_cert.sh]
ports: ["8100 (LAN, HTTP — HA integration)", "8443 (LAN, HTTPS — browser mic, PWA /app/)"]
health: http://voice:8100/health
docs: [docs/mobile-chat-app.md, docs/voice-call.md, docs/services.md]
depends_on:
  - to: engine
    why: "conversation replies (OLLAMA_URL)"
  - to: engine-igpu
    why: "fallback replies while a job is running"
  - to: memory
    why: "search_jobs tool"
  - to: jarvis-jobs
    why: "schedule_job tool and the worker-busy signal (job store mount)"
  - to: home-assistant
    why: "the jarvis_voice HA integration posts here"
  - to: engine-voice
    why: "conversational model during a voice call window"
---

## Purpose

FastAPI service: faster-whisper (small) STT, the brain (persona
`JARVIS_VOICE`, default `jarvis`), edge-tts TTS. Serves the browser voice
chat (`/voice/chat`), the mobile PWA (`/app/`), text chat (`/chat`) and the
HA integration endpoints. Brain tools: `schedule_job`, `search_jobs`. Owns
the voice-call session state machine (docs/voice-call.md).

## Interfaces

- `GET /health`, `GET /models`, `POST /stt`, `POST /voice`,
  `POST /voice/json`, `POST /chat`, `WS /voice/ws`, `/voice/chat`, `/app/`.
- The reply text also rides in the `X-Reply` header (control characters
  collapsed — a newline there once aborted every response, 2026-10-27).
- `WS /voice/ws` streams: the reply's LLM round runs with `stream: true`
  and each completed sentence is synthesized (edge-tts) and sent as one
  small WAV binary frame as soon as it is done — first audio lands after
  the first sentence, not after the whole pipeline (2026-11-26).
- `POST /voice/json` with request header `X-Audio: 0` returns a JSON body
  (`transcript`, `reply`, `session`, `elapsed`) and skips the edge-tts
  synthesis — the HA satellite sends it (it reads only `X-Reply` and speaks
  with HA's own TTS). No/other header keeps the old audio body.

## Configuration

- Compose env: `OLLAMA_URL`, `FALLBACK_OLLAMA_URL`, `FALLBACK_MODEL`,
  `MEMORY_URL`, `WHISPER_MODEL`, `JARVIS_VOICE`, `LISTEN_PORT`.
- `env_file`: `~/.config/jarvis/local.env`, then
  `/opt/jarvis-voice/auth-off.env` (auth is off because the HA integration
  sends no credentials).
- TLS cert: `~/.config/jarvis/tls` (ro) — `scripts/gen_voice_cert.sh`;
  re-run if the box IP changes.

## Data

- Code is bind-mounted from the pinned deploy point `/opt/jarvis-voice`
  (`DEPLOYED_FROM` marker). Whisper models from `~/.cache/huggingface` (ro).

## Deploy

Copy the service code to `/opt/jarvis-voice`, update `DEPLOYED_FROM`, then

    docker compose build voice && docker compose up -d voice

The image needs ffmpeg (webm/opus decode and resampling).

## Verify live

    curl -s 127.0.0.1:8100/health
    curl -sk https://127.0.0.1:8443/app/ -o /dev/null -w '%{http_code}\n'

Then a real turn: POST a short WAV and a webm/opus clip to `/voice/json`
and check transcript + reply + audio.

Streaming verification (inside the container):

    docker exec jarvis-voice python3 /opt/jarvis-voice/scripts/voice_stream_test.py

It checks the REST X-Audio behavior, the WS time-to-first-audio (first
frame must land well before the final idle status), and the sentence
streaming of run_tool_loop against a local fake SSE server.

## Failure modes

- Mic never worked: buffer size, sample rate, binary frames, raw PCM (5
  bugs, 2026-10-27 PR #69).
- HTTPS dropped by containerization → browser blocks the mic silently
  (2026-10-27, PR #90).
- No ffmpeg in the image → every `/voice` turn 500'd (found 2026-11-18,
  PR #263) although the webm fix (PR #78) relied on it since 10-27.
- The HA satellite speaks through HA's own TTS (google_translate), not
  this service's edge-tts — tuning TTS here does not change the satellite
  voice (2026-11-16). It used to pay for that dead edge-tts pass on every
  turn; it now sends `X-Audio: 0` (2026-11-26).
- `/voice/chat` never played the audio: the WS client checked
  `instanceof Blob` while binary frames arrive as ArrayBuffer
  (binaryType default). Fixed with `ws.binaryType = 'arraybuffer'` +
  ArrayBuffer handling (2026-11-26, unverified in a real browser).
