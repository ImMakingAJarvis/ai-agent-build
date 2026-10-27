# On-Device openWakeWord Confirmation — Voice Preview Edition

**Date:** 2026-10-26
**Job:** phase-4-confirm-on-device-openwakeword-detection-is-working
**Hardware:** VPE voice device (on-device openWakeWord) → Home Assistant
(111.1.2.200:80) → `jarvis_voice` agent → Jarvis voice service
(111.1.2.27:8100)
**Wake word:** "okay jarvis"

## Verdict

**CONFIRMED, with caveats — on-device openWakeWord detection is working.**
In 2 of 3 live hands-free rounds the VPE reacted to the owner's real
speech: round 2 produced the direct on-device reaction (listening light +
wake acknowledgement sound), and round 1 produced a full pipeline run
through to the voice service (only possible after the wake word fires).

The wider VPE voice path is currently **intermittent**: round 3 produced no
device reaction at all, round 2's detection never reached the agent, and
reply playback is inconsistent (the owner "thought he heard something talk
the first time"). Detection works; reliability is a separate, queued
defect.

This is distinct from the 2026-10-22 test
(`openwakeword-test-results.md`), which only proved the model loads and
processes audio on the *build machine* with synthetic espeak audio. This
document is the on-device confirmation with real speech.

## Method

Three controlled rounds. For each round the owner stood in front of the VPE
speaker, hands-free, with all screens/apps closed, and said:

> "okay jarvis, the sky is blue"

While he spoke, the voice service journal was tailed live
(`journalctl -u jarvis-voice -f`) and every request was timestamped. A
request from HA (source IP 111.1.2.200) to `POST /voice/json` can only be
produced by HA's voice pipeline invoking the `jarvis_voice` conversation
agent, which only happens after openWakeWord fires on the VPE and the
utterance is transcribed.

## Results

| Round | Time (local) | Device reaction | Request reached voice service | Owner report |
|-------|-----------|-----------------|-------------------------------|--------------|
| 1 | 01:25:37–01:25:43 | — (pipeline ran) | **Yes** — `POST /voice/json` from 111.1.2.200, 200 OK, LLM reply generated | "no reply" (round 3: "I think I heard something talk the first time?") |
| 2 | ~02:26 | **Yes** — listening light spun, acknowledgement sound, then pulsed and stopped | No | described above |
| 3 | 02:59–03:24 (window) | No — device silent | No — zero journal entries in the whole window | "Said it, silent" |

### Round 1 log evidence (service side)

```
Oct 26 01:25:37 ... 111.1.2.200:56732 - "POST /chat HTTP/1.1" 404 Not Found
Oct 26 01:25:38 ... LLM reply: It is good to see you again. I assume you have
                    a very important task for me, or you are simply enjoy...
Oct 26 01:25:43 ... 111.1.2.200:56732 - "POST /voice/json HTTP/1.1" 200 OK
```

The 404 on `/chat` is a benign probe; the agent then succeeded via
`/voice/json`. A real transcript reached the LLM and a full reply was
returned to HA.

Attribution: this request arrived during the owner's device test (he
reported using the speaker, hands-free). The web chat page he had briefly
open uses the `/voice/ws` WebSocket endpoint and cannot produce
`/voice/json`, so the only remaining alternative source would be HA UI
push-to-talk, which he did not use in that window. Combined with round 2's
direct device reaction, the wake word is confirmed as the trigger.

## What is working

- openWakeWord fires on real room speech on the VPE (light + ack sound).
- Wake-word trigger reaches HA and invokes the `jarvis_voice` agent
  (round 1: full pipeline, 200, ~6 s).

## What is NOT working (separate defects, out of scope for this job)

1. **Intermittent device reaction.** Round 3: the owner said the wake word
   but the VPE did not react at all — no light, no sound, and zero voice
   service log entries in the entire window (nothing reached HA at all).
   Either the detector did not fire, or the VPE was stuck in a bad state
   (round 2 ended with it "pulsing until it stopped").
2. **Unreliable VPE → HA handoff.** Round 2: the detector fired (light +
   ack) but no conversation reached the voice service — the utterance died
   between the VPE and the agent (likely empty STT result or pipeline error
   on the HA side; needs HA logs to pin down).
3. **Unreliable reply playback.** Round 1: the agent returned a 200 with
   reply text; the owner first reported no audio, then added "I think I
   heard something talk the first time?" — the TTS/playback leg of the
   VPE/HA pipeline delivers audio at best intermittently.

These explain the owner's long-standing report "the wake word activates,
but it just spins and eventually stops." They are queued as follow-up work,
not part of this confirmation.

## Follow-ups suggested

- Job: debug VPE → HA handoff (round 2) — needs HA logs or a long-lived HA
  access token (API on 111.1.2.200:80 returns 401; no token available on
  this box).
- Job: fix VPE reply playback (TTS leg).
- Optional: log the incoming transcript in the voice service
  (`/voice/json`) so future checks can show exactly what the device heard.
