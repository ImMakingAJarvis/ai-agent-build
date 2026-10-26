# Jarvis Voice Home Assistant Integration

Connects Home Assistant's voice pipeline to the local Jarvis voice service
(STT → LLM → TTS) so wake-word triggers produce spoken replies.

## Architecture

```
[Voice Preview Edition]
        │  wake word detected → on-device STT → text
        ▼
[Home Assistant]
        │  routes text to the jarvis_voice conversation agent
        ▼
[Jarvis Voice Service :8100]
        │  POST /voice/json  (text in → reply text out)
        ▼
  brain (llama.cpp, OpenAI-compatible) → X-Reply
        │
        ▼  reply text
[Home Assistant] → HA-side TTS → plays back on speaker
```

There is also a raw audio path, `POST /voice` (audio in → TTS audio out),
used by the web chat page and by clients that do their own STT. On that path
the service runs faster-whisper (CPU) for STT and edge-tts for TTS.

## Installation

### 1. Copy the integration

Copy the `jarvis_voice` directory into your Home Assistant
`custom_components` folder:

```bash
cp -r integrations/jarvis_voice \
  /path/to/homeassistant/config/custom_components/
```

Or, if Home Assistant runs in Docker and the voice service is on the host:

```bash
# On the Docker host, assuming config is mounted at /ha/config
cp -r integrations/jarvis_voice \
  /ha/config/custom_components/
```

### 2. Restart Home Assistant

```bash
ha core restart
# or reload custom integrations from Developer Tools → YAML
```

### 3. Configure

Go to **Settings → Devices & Services → Add Integration** and search for
**Jarvis Voice**. Enter the voice service URL and its credentials:

- URL — local install: `http://localhost:8100`; Docker HA talking to host:
  `http://host.docker.internal:8100`; same LAN: `http://<server-ip>:8100`
- Username / Password — the `VOICE_USER` / `VOICE_PASS` values the voice
  service was started with (see `~/.config/jarvis/local.env`). Leave blank
  only if the service is running without auth.

The config flow pings `/health` with those credentials to verify the service
is reachable and the credentials are accepted before saving.

### 4. Set as default conversation agent

**Settings → Voice Assist → Conversation agents** → select **Jarvis** as
the default agent.

### 5. Test

Say the wake word. The full pipeline should respond within ~12 seconds:

1. openWakeWord detects "okay Jarvis" on the Voice Preview Edition
2. HA routes the audio clip to the `jarvis_voice` agent
3. The agent POSTs audio to `http://<server>:8100/voice`
4. The service transcribes → LLM → TTS → returns WAV
5. HA plays the audio back through the speaker

## Alternative: REST Command (no custom integration)

If you prefer not to install a custom component, you can use HA's built-in
REST command integration. Add to `configuration.yaml`:

```yaml
rest_command:
  jarvis_voice:
    url: "http://<server-ip>:8100/voice"
    method: POST
    content_type: "multipart/form-data"
    payload: "{{ '' }}"
  jarvis_voice_text:
    url: "http://<server-ip>:8100/voice/json"
    method: POST
    content_type: "application/json"
    payload: '{{ {"text": "{{ text }}"} | to_json }'
  jarvis_reset_history:
    url: "http://<server-ip>:8100/history/reset"
    method: POST
```

Then trigger with an automation:

```yaml
automation:
  - alias: "Jarvis voice pipe"
    trigger:
      - platform: event
        event_type: "wake_word_detected"
    action:
      - service: rest_command.jarvis_voice_text
        data:
          text: "{{ trigger.event.data.text | default('Hello Jarvis') }}"
```

## Services

| Service | Description |
|---------|-------------|
| `jarvis_voice.reset_history` | Clear the in-memory conversation history |

## Endpoints

The Jarvis voice service exposes these endpoints:

| Method | Path | Input | Output |
|--------|------|-------|--------|
| `POST` | `/voice` | Audio file (multipart) | WAV audio |
| `POST` | `/voice/json` | `{"text": "..."}` | WAV audio |
| `POST` | `/stt` | Audio file | `{"text": "..."}` |
| `POST` | `/tts` | `{"text": "..."}` | WAV audio |
| `POST` | `/history/reset` | — | `{"status": "history cleared"}` |
| `GET`  | `/health` | — | `{"status":"ok","whisper":true,"llm":true,"voice":"glados",...}` |

Response headers on `/voice` and `/voice/json`:
- `X-Transcript` — what was transcribed
- `X-Reply` — what the LLM replied
- `X-Elapsed` — total pipeline time in seconds

## Voice profiles

The voice service runs one of the personas defined in
`scripts/voice_prompts/`, selected by the `JARVIS_VOICE` env var on the
`jarvis-voice` systemd unit:

| Profile | Persona | edge-tts voice | Notes |
|---------|---------|----------------|-------|
| `jarvis` | Jarvis, the helpful home AI | `en-US-GuyNeural` | the stock voice |
| `glados` | GLaDOS from Portal: dry, sarcastic, unenthusiastic | `en-US-EmmaNeural` at -10% rate, -2Hz pitch | flat, deadpan delivery |

Overrides (env vars on the unit): `VOICE_PROMPT` (prompt file),
`TTS_VOICE`, `TTS_RATE`, `TTS_PITCH` (edge-tts settings). After changing
any of them: `sudo systemctl restart jarvis-voice`.
