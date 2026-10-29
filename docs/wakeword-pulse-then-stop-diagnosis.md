# openWakeWord "pulse then stop" — diagnosis

**Date:** 2026-10-27
**Job:** chat-1793008053-3f25 — Diagnose openWakeWord pulse-then-stop behavior
**Device:** ESPHome assist satellite `home_assistant_voice_09897c` (VPE, on-device
openWakeWord, wake word "Hey Jarvis") → HA 2026.10 @ 111.1.2.200:80 → "Jarvis"
Assist pipeline → `jarvis_voice` HTTP conversation agent → voice service
(111.1.2.27:8100)
**Sources:** HA `/api/states` (live) + history/logbook (111.1.2.200), the
`homeassistant` MCP server (live context from this box — device domains like
`light`; it does not expose `stt` entities or Assist-pipeline config),
`journalctl -u jarvis-voice` + Docker `jarvis-voice` logs (this box),
companion docs `runner-log-tail-2026-10-27.md`,
`phase4-wake-test-error-light-capture.md`,
`openwakeword-vpe-confirmation.md`. No code changes.

## TL;DR

**Likely cause:** the STT in the "Jarvis" Assist pipeline,
`stt.speech_to_phrase`, is **dead** (entity state `unknown`; dead since ~Oct 23).
The wake word fires and the VPE→HA handoff works, but the utterance is never
transcribed, so the pipeline stalls at `listening` and times out (7–20 s) back to
`idle` with no reply. The LED ring "pulse" is the device-local listening
animation; it stops because the pipeline never advances past `listening`.

**One fix:** give the "Jarvis" pipeline a **working STT** — load the already
installed (but not loaded) **Whisper** STT on HA and set it as the pipeline's
STT, replacing the dead `stt.speech_to_phrase`. (Equivalent alternative: repair
/reinstall the speech-to-phrase addon.) Both current STT entities are `unknown`,
so the pipeline has no live transcriber until one is connected.

## The last 2 test runs (event traces, all PDT)

| Loop | Wake → listening → end | Outcome |
|---|---|---|
| **Oct 26 23:45:12–22** | wake burst (1 s) → `listening` 9.4 s → `idle` | no reply |
| **Oct 26 23:46:51–59** | wake burst (1 s) → `listening` 7.6 s → `idle` | no reply (last activity) |

Both: openWakeWord detects "Hey Jarvis" → VPE plays the ack beep and spins the
LED ring (satellite → `listening`) → the device waits for the command to be
transcribed → **no transcription returns** → `listening` times out → `idle`.
That is exactly the owner's "beep + light spin, then it pulses and stops with no
response."

The same stall repeats in every loop in the captured window: Oct 26
16:30:39→16:31:00 (20 s), 17:35:46, 17:56:25, 20:35:26, 23:45:12, 23:46:59, and a
satellite/media-player state change at **Oct 27 13:03** (zero voice-service
requests — same dead-STT signature). The single exception, Oct 26 21:18:14,
reached `processing`→`responding` and produced a ~2 s HA-side
`tts.google_translate` clip with **zero calls to the local voice service** — an
error/noise clip from the pipeline, not a real answer, consistent with an STT
failure being spoken rather than the owner's utterance.

## What is working vs broken

| Stage | Status (verified 2026-10-27) |
|---|---|
| openWakeWord wake detection (ESP32, on-device) | **Working** — fires every time (satellite → `listening`, ack beep, LED spin) |
| VPE → HA handoff (satellite states) | **Working** — state transitions reach HA reliably |
| HA STT (`stt.speech_to_phrase`) | **DEAD** — state `unknown`; no transcription ever returns |
| "Jarvis" conversation agent → voice service (8100) | Never invoked — no transcript to send (agent last used Oct 26 01:15) |
| Local brain (llama, 11434) + voice service (8100) STT/LLM/TTS | **Healthy now** — Docker `jarvis-engine` up 3 h, `jarvis-voice` up; web-chat STT+LLM round-trips succeeded Oct 27 21:22–21:28 |

Because the local brain and voice service are healthy, **the only broken leg is
HA's STT.** The silence is entirely on the HA side.

## Root cause

`stt.speech_to_phrase` (the "Jarvis" pipeline's STT) has been `unknown` since
~Oct 23 and flips unavailable↔unknown on every HA restart — it never comes up.
With no live STT, the Assist pipeline cannot turn the captured audio into text,
so it never invokes the conversation agent, never calls the voice service, and
never produces TTS. The satellite sits in `listening` until its 8–20 s timeout
and returns to `idle`. Nothing on the build box crashed; the local journal shows
zero voice requests in every one of these windows.

## The fix

Connect a working STT to the "Jarvis" pipeline. Primary option:

1. **Load the Whisper STT on HA** (noted in the Oct 26 diagnosis as
   "installed but not loaded") and **set it as the STT for the "Jarvis" Assist
   pipeline**, replacing `stt.speech_to_phrase`.

Equivalent alternative: repair/reinstall the speech-to-phrase addon so
`stt.speech_to_phrase` becomes available.

This box **does have HA access** — the `homeassistant` MCP server (live context
+ device control) plus the REST and WebSocket API — and that is how this
diagnosis was re-verified live on Oct 27 (both STT entities still `unknown`,
satellite `idle`, pipeline selection still "Jarvis"). But the Assist-pipeline
/ STT config is not *settable* through any of those interfaces: the MCP toolset
has no STT/pipeline tools (and does not expose `stt` entities in live context),
REST `/api/assist_pipeline` → 404, and WS `assist_pipeline/list` →
`unknown_command` (HA 2026.9.4). So the config change itself (load the Whisper
STT / re-point the pipeline STT / repair the addon) still has to be made **on
the HA box** (UI or Supervisor); the result is verifiable from here afterwards.
After the STT is live, re-test "Hey Jarvis, the sky is blue": the satellite
should advance `listening → processing → responding`, a `POST /voice/json`
should appear in the `jarvis-voice` journal, and a TTS reply should play on the
VPE speaker.

## Re-verification (2026-10-27, job chat-1793164047-6316)

Re-checked live from this box after the owner pointed out the MCP `homeassistant`
server: it works (live context for device domains such as `light`), confirming
this box can *see* HA. REST `/api/states` re-check: `stt.speech_to_phrase` and
`stt.nathan_s_iphone` both still `unknown` (last changed 2026-10-27 01:25 / 02:41
UTC), satellite `idle`, `select.…_assistant` still "Jarvis". Diagnosis holds.
Also confirmed the pipeline/STT config cannot be set from here by any available
interface (MCP toolset: no STT/pipeline tools; REST `/api/assist_pipeline` → 404;
WS `assist_pipeline/list` → `unknown_command`).

## Limitations

- openWakeWord's own logs live on the ESP32 VPE / HA box (ESPHome serial) — not
  reachable from this box; wake detection is confirmed via the satellite state
  transitions instead.
- HA logbook is empty now (cleared by HA restarts + limited in-memory
  retention); the per-event traces above are from the 11 h history capture in
  `runner-log-tail-2026-10-27.md` plus live `/api/states` `last_changed`
  timestamps.
- The exact HA-side STT error text is in `home-assistant.log` on the HA box
  (standing open item, needs SSH/Supervisor).
