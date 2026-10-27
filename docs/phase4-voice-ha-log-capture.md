# Voice + HA Log Capture — Last 3 Wake-Word Tests

**Date:** 2026-10-26
**Job:** chat-1793010447-6d93 — capture voice+HA logs from last 3 wake-word tests
**Sources:** `journalctl -u jarvis-voice` (this box) + Home Assistant 2026.9.4 at
111.1.2.200:80 (REST + websocket: state history, logbook, entity
`last_triggered`). Companion to `openwakeword-vpe-confirmation.md` (last night's
3 controlled rounds) and the 16:45 pipeline diagnosis (job
chat-1793010301-a80f, which found the dead STT).

## TL;DR — the two questions

**What text/TTS went to the Sonos: nothing.** No TTS or voice-reply event in
the HA data correlates with any Sonos playback. All TTS/playback landed on the
VPE's own speaker (`media_player.home_assistant_voice_09897c_media_player`,
1–16 s bursts) or, once, on the `tts.nathan_s_iphone` engine (01:29:22–57, see
below). Sonos Roam was playing music (alexxii airplay 12:51–14:12, Spotify
"Crash" after 14:12) and Sonos Living Room was idle throughout. The
jarvis_voice replies (edge-tts, GLaDOS profile, en-US-EmmaNeural) were returned
to HA as WAV; the 16:04 one has no matching VPE playback event (consistent
with the known playback defect). Caveat: the VPE's own config is not readable
from this box (API only, no SSH), so a device-level cast to Sonos can't be
100% excluded — but no HA-side event indicates it.

**What command hit the HA light entity:**

| Time (PDT) | Event |
|---|---|
| 03:21:52 | `light.staircase_foyer_stairs` → **on** (same second as a VPE ack burst) |
| 03:24:28 | whole-house → **on**: `light.house`, `light.downstairs`, `light.dining_dining` + ~30 member lights, same instant as a `tts.google_translate_en_com` synthesis |
| 16:21:07 | state-restore cluster (foyer stairs on, most others off) — HA restarted ~16:21:10 (VPE/Sonos/TTS entities went unavailable→back); **not a voice command** |
| 01:37:17 | similar restore cluster from last night's HA restart; **not a voice command** |

No automation, script, or scene fired in any test window (every
`automation.*`/`script.*` `last_triggered` checked — nearest were the
per-minute camera/timelapse automations and one on 10-24). So the 03:21:52 /
03:24:28 commands came either from HA's built-in conversation agent (LLM tool
call → `light.turn_on`) — the 03:24:28 one looks like a single group-level
`light.turn_on` — or from the owner acting in the HA app. Which one cannot be
distinguished without `home-assistant.log` on the HA box (no SSH from here).
Today's retest produced **no** voice-driven light-entity command. The
"lights spun around" the owner reported at 16:44 was the VPE's own LED ring
(`light.home_assistant_voice_09897c_led_ring` shows only unavailable/off —
the spin is device-firmware behavior, not an HA entity state change).

## Test-utterance timeline (all times 2026-10-26 PDT)

Spoken utterances themselves are **not in any log**: HA's local STT
(`stt.speech_to_phrase`, dead since ~Oct 23 per the 16:45 diagnosis) never
transcribes what the owner says after the wake word, and the voice service
does not log the input text of `/voice/json` (only the LLM reply, 100 chars).
What the logs *do* show, newest first:

**1. 16:04:26–16:04:35 — "the sky is blue" via HA (API test, not spoken).**
`POST /chat` 404 probe, then `POST /voice/json` 200 from 111.1.2.200;
`LLM reply: I am aware. It is blue. If you are testing my ability to detect
repetition, you have succeeded.` — source IP is the HA box and the 16:45
diagnosis round tested the agent by calling HA's conversation API directly
with that exact phrase, so this entry is that test, not a VPE wake test.
No VPE playback event follows (no bursts between 16:04:35 and 16:11:55).

**2. 16:11:54–16:30:39 — onboarding re-run (owner's "speaker test" session).**
16:11:54 `tts.piper` synthesis → VPE "playing" 16:11:55–16:12:11 (16 s — the
speaker-test sentence the owner heard at 16:13). 16:21:07 light restore +
HA restart. 16:26:49 / 16:27:59 / 16:30:10 short VPE bursts (ack-style).
16:30:31 `tts.google_translate_en_com` synthesis → VPE playing 16:30:32–39
(7 s — the "asking for a response" message the owner heard at 16:33).
These are HA→owner prompts, not owner utterances; zero requests reached the
voice service in 16:10–16:13.

**3. 03:21:51–03:24:30 — last night, end of round 3 (owner's real speech,
untranscribed).** VPE ack bursts at 03:21:51, 03:22:09, 03:22:28, 03:23:12,
03:23:31 (5 wake reactions — more than the confirmation doc recorded, which
had round 3 as "device silent"); `light.staircase_foyer_stairs` on at
03:21:52; whole-house lights on + `tts.google_translate_en_com` at 03:24:28;
final bursts through 03:28:14. **Zero voice-service entries 03:15–03:30.**
Because the local STT was already dead, the light commands could not have
come from a transcribed VPE utterance — most likely the built-in HA agent
handling an app-side (typed/push-to-talk) "turn on the lights", or owner
action in the app.

Context from the same logs:

- 01:14:18–01:16:03 — four jarvis_voice exchanges from 111.1.2.200
  (GLaDOS greeting / time / kitchen / identity; replies in the journal,
  input text unlogged), 200 OK each; 01:20:48 `/history/reset` from local.
- 01:25:37–43 — round 1 of the confirmation doc ("okay jarvis, the sky is
  blue") → jarvis_voice, 200; reply TTS path unconfirmed (01:26:15 VPE burst
  is 1 s — likely an ack, not the reply).
- 01:29:22/38/57 — `tts.nathan_s_iphone` synthesized 3 times (~16 s apart).
  Unexplained; if the VPE's TTS target was the phone at that moment this is
  a candidate for the "reply went somewhere I can't hear" symptom.
- 13:02 / 13:43 / 14:53 / 15:23 / 15:49 — the four-plus-one listening
  sessions of the 16:45 diagnosis: 1–2 one-second VPE bursts each (wake acks)
  then nothing, matching "wake word fires, then silence".
- 15:18:02 / 15:20:09 — local (127.0.0.1) `/tts`, `/stt` and a "Rayleigh
  scattering" `/voice/json` exchange: earlier-round local API test, not a
  wake test; excluded.
- HA restarts ~01:37 and ~16:21 (entity unavailable→available waves) explain
  both light restore clusters and the VPE flaps at 03:54/05:27/07:33/12:23/
  13:20/13:58 (VPE disconnects).

## What this pins down

- Wake acks reach HA and the VPE media player reliably; **no owner utterance
  has reached the voice service since the STT died** (~Oct 23). The 16:04
  entry is an API test, the 03:21–03:24 light commands are app/built-in-agent
  side effects, and the onboarding prompts (piper 16:11, google_translate
  16:30) are HA→owner.
- TTS engines in play: piper (VPE onboarding), google_translate (03:24,
  16:30), nathan_s_iphone (01:29). None routed to Sonos.
- The 03:24:28 whole-house lights-on is the only real "command hit the light
  entity" in the capture window; it happened 24 min after the owner's last
  confirmed wake test, via a non-VPE path.

## Open / follow-ups

1. Read `home-assistant.log` on the HA box (needs SSH or the Supervisor
   terminal) to name the exact `light.turn_on` caller at 03:21:52/03:24:28
   and the 01:29 iPhone-TTS target.
2. Log the incoming text in the voice service `/voice/json` (one
   `logger.info("Inbound: %s", req.text)` line) so future captures show what
   the pipeline heard — gap already flagged in the confirmation doc.
3. Fix the dead `stt.speech_to_phrase` (or point the pipeline at a working
   STT) — the 16:45 diagnosis's root cause; everything above is a symptom
   of it plus the playback leg.
