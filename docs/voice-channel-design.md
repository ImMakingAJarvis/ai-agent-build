# Voice channel design: camera mics + direct speakers (no Home Assistant)

**Status:** design approved for build-out — see the kanban epic
"Own voice channel: camera mics + direct Sonos/Google speakers (no Home
Assistant)" (`ed9ecd12-6d2d-4342-92df-93f46312d8d2`, 12 tasks in dependency
order) for the work breakdown.

**Origin:** Nathan, 2026-10-27 — "instead of going through home assistant,
design our own voice channel, where you can use the microphones from the
security cameras, and the Sonos or Google speakers in the house. Future
microphones will come."

## 1. Why

Today the whole audio path rides on Home Assistant:

- **Input:** an HA Voice Preview Edition satellite does wake word + STT and
  POSTs the transcription to the Go wake service (`internal/wake`,
  `:8081/wake`); the `jarvis_voice` HA integration (v0.2.0, hand-installed,
  auth off) POSTs audio clips to `jarvis-voice`.
- **Output:** `jarvis-voice` streams the TTS reply back to HA, which plays
  it on a speaker through its own media_player entities.

That means the audio quality, latency and which room can talk to Jarvis are
all limited by HA's satellite hardware and HA's media pipeline. We already
run the whole brain locally (wake word models, faster-whisper, jarvis-brain,
edge-tts all live on this box), so the only HA-owned piece is *capture and
playback*. This design replaces just that piece.

**Non-goals:** HA remains the source of truth for lights/climate/locks and
the rest of the house. We only bypass HA for the audio path. The existing
`jarvis_voice` integration keeps working as a legacy/parallel path (the
PWA and the satellite keep functioning unchanged).

## 2. Target architecture

```
 UniFi Protect NVR (RTSP audio, one stream per enabled camera)
        │  PyAV: audio track → 16 kHz mono PCM16
        ▼
 ┌─────────────────────── jarvis-voice (scripts/voice_service.py) ───────────────────────┐
 │  channel layer (new, config-driven)                                                   │
 │                                                                                        │
 │  MicSource (registry)        wake loop per mic           existing core (unchanged)     │
 │  ┌───────────────┐          openwakeword + VPE VAD ──►  STT (faster-whisper)          │
 │  │ unifi_rtsp    │ ─────► ring buffer, pre-roll        → brain (jarvis-brain,         │
 │  │ usb (future)  │        pre-roll + VAD endpoint      GLaDOS profile)                │
 │  │ esp32 (later) │          "okay jarvis" / "hey       → TTS (edge-tts, MP3)          │
 │  └───────────────┘          jarvis"                          │                         │
 │                                                              ▼                         │
 │                                   room router + SessionManager (per-room sessions,     │
 │                                   barge-in, idle timeout, echo guard, failover)        │
 └──────────────────────────────────────────────────────────────┼──────────────────────────┘
                                                                ▼
        SpeakerSink (registry) ──► Sonos (S1 RCP /announce, ducks music)
                                  Google Nest (Cast protocol)
                                  UniFi talk API (camera / doorbell speakers)
```

Everything new lives in the existing `jarvis-voice` service (started as an
asyncio background task on boot); no new service, no new ports. The
service's public endpoints (`/voice`, `/stt`, `/tts`, `/voice/ws`, `/app`,
`/health`) are unchanged.

## 3. Components

### 3.1 MicSource (input)

Interface: `id`, `room`, `open() -> frames of 16 kHz mono PCM16 (100 ms)`,
`close()`, `health()` (last frame timestamp). A registry is loaded from
config; each source runs its own wake loop in a task.

- **`unifi_rtsp`** — pull the camera's RTSP stream from the Protect NVR
  (audio track via PyAV/ffmpeg), resample to 16k mono. Reconnect with
  exponential backoff. This is the first real source: the house cameras are
  UniFi Protect (front/back door, art room, living room, sunrise, mom's
  doorbell, G5 bullet), all with mics.
- **`usb`** — `sounddevice` (already installed). Shipped as the *reference
  adapter* so every future microphone (USB mic, satellite, ESP32 node) is
  either a config entry or a ~50-line adapter, never a redesign.
- **future** — ESP32 mic nodes (openWakeWord already runs on-device there),
  additional NVRs, satellites: same interface, added via config.

**Why RTSP instead of the HA camera entities:** the HA entities are
video-only proxies of the same NVR; the NVR's RTSP stream carries the audio
track, and pulling it directly keeps the channel independent of HA entirely.

### 3.2 Wake loop

Per mic stream: openWakeWord (installed; the custom `hey_jarvis_v0.1.onnx`
model plus the confirmed "okay jarvis" phrasing from
`docs/openwakeword-vpe-confirmation.md`) + VPE VAD. Ring buffer keeps 1 s of
pre-roll; on wake, capture until the VAD endpoint (1.5 s min / 10 s max) and
hand the clip to the existing `transcribe()` path.

Latency budget (targets, baselines in `docs/wake-latency-measurements.md`):

| stage | target |
|---|---|
| wake → STT start | < 2 s |
| wake → first spoken word | < 5 s (end-to-end) |

Echo guard: a mic in a room whose speaker is currently playing TTS is
suppressed (camera mics are in the same room as their own speakers — this
matters for doorbells). Rate limit: 1 wake per mic per 3 s.

### 3.3 SpeakerSink (output)

Interface: `id`, `room`, `play(audio, fmt, duck=True)`, `stop()`,
`health()`.

- **`sonos`** — S1 RCP protocol (port 1400, XML over HTTP, no pip dep):
  zone discovery, volume control. TTS goes out via the RCP **announce**
  endpoint (Sonos' own low-latency talk path, used by doorbells — it ducks
  music automatically and doesn't require stopping the zone). Fallback:
  pause → stream MP3 → resume. Music duck: save volume, ~30% while speaking,
  restore on stop/timeout.
- **`cast`** — Google Nest speakers via the Cast protocol (port 8009,
  `cast_media`): mDNS discovery, MP3 load, volume, stop. Audio speakers
  only; Chromecast TVs are skipped unless a config entry opts in.
- **`unifi_talk`** — Protect NVR talk API (PCM16 → camera speaker). This is
  what makes doorbell conversations work: the front-door mic answers on the
  front-door speaker, exactly like a video doorbell, with no HA involved.

### 3.4 Room router + sessions

Config maps `room -> [mics] -> [speakers in priority order]`. A
`SessionManager` handles the conversation state:

- wake in room R → pick the first **healthy** speaker in R's list → play.
- **barge-in:** a new wake in the same room while TTS is playing stops the
  reply and starts a fresh one.
- **idle timeout:** 15 s of silence closes the session (and restores music
  volume).
- limits: 1 session per room, 2 global; every transition logged.

## 4. Configuration

`~/.config/jarvis/voice-channels.json` (secrets — NVR account, anything
else — live in env files under `~/.config/jarvis/`, never in this repo):

```json
{
  "nvr": {"host": "111.1.2.x", "port": 7441, "account_env": "JARVIS_NVR_ACCOUNT"},
  "mics": [
    {"id": "front_door", "type": "unifi_rtsp", "camera": "<sn>", "room": "front_door", "enabled": true},
    {"id": "living_room_cam", "type": "unifi_rtsp", "camera": "<sn>", "room": "living_room", "enabled": false}
  ],
  "speakers": [
    {"id": "front_door_cam", "type": "unifi_talk", "camera": "<sn>", "room": "front_door"},
    {"id": "living_room_sonos", "type": "sonos", "zone": "Living Room", "room": "living_room"},
    {"id": "kitchen_nest", "type": "cast", "host": "111.1.2.x", "room": "kitchen"}
  ],
  "rooms": [
    {"room": "front_door",  "speakers": ["front_door_cam", "living_room_sonos"]},
    {"room": "living_room", "speakers": ["living_room_sonos", "kitchen_nest"]},
    {"room": "kitchen",     "speakers": ["kitchen_nest"]}
  ]
}
```

**Adding a microphone (the "future microphones will come" requirement):**
enable an existing camera in `mics` (one JSON entry) or add a new
`type` adapter if the hardware is new — either way the wake loop, STT,
brain, TTS, routing and speakers are untouched.

## 5. House inventory (as of 2026-10-27; T1 refines this)

Mics available (UniFi Protect cameras, all with built-in mics):

| camera (HA entity) | room |
|---|---|
| front door | front_door |
| back door | back_door |
| G3 Instant | art room |
| G4 Instant | living room |
| G5 Bullet | (to be assigned) |
| G4 Doorbell Pro | sunrise |
| Doorbell Lite | mom's doorbell |

Speakers available (direct protocols):

| kind | examples (HA friendly names) | protocol |
|---|---|---|
| Sonos | Sonos Living Room, Princess, … | S1 RCP (:1400) |
| Google Nest | Kitchen Nest, Main Bedroom Nest, Nathan's room, … | Cast (:8009) |
| UniFi camera speakers | Front/Back Door Speaker, Art Room Camera Speaker, G4 Doorbell Pro Speaker, Mom's Doorbell Speaker | Protect talk API |

(HomePods, LG TVs and the HA Voice satellite are out of scope for the
direct channel; they stay reachable through HA.)

## 6. Risks / open questions (T1 resolves these)

1. **NVR reachability from the agent box.** A 2026-10-27 scan of
   111.1.2.0/24 found nothing on :7441 — the NVR may be on another VLAN or
   firewalled from the agent box. Fallbacks: run capture where the NVR is
   reachable, or an allow rule. Must be settled before T2.
2. **RTSP audio codec/latency.** Protect RTSP audio codec and end-to-end
   latency are unmeasured; T1 records real clips and measures.
3. **Sonos S1 vs S2.** S1 RCP (:1400) is the stable, dependency-free path;
   if a zone only responds on S2, `cast`-style fallback or HA media_player
   for that zone.
4. **Camera mic → own speaker echo.** Handled by the echo guard, but
   doorbell hardware (chime vs talk) needs the T8 verification.

## 7. Roadmap (kanban tasks, dependency order)

| # | task | model |
|---|---|---|
| T1 | Device inventory & reachability probe | sonnet |
| T2 | MicSource interface + UniFi RTSP source | sonnet |
| T3 | Wake word loop per mic stream | sonnet |
| T4 | SpeakerSink interface + Sonos sink | sonnet |
| T5 | Google Nest (Cast) sink | sonnet |
| T6 | Room router + session manager | sonnet |
| T7 | End-to-end wiring into jarvis-voice | sonnet |
| T8 | UniFi Protect talk sink (camera speakers) | sonnet |
| T9 | Deploy, config, docs | local |
| T10 | USB mic source (template for future mics) | local |
| T11 | E2E test harness + latency benchmark | local |
| T12 | Multi-room polish (stretch) | sonnet |

Each task on the board carries its own detailed instructions and
acceptance criteria; the epic description is the one-paragraph summary of
this document.
