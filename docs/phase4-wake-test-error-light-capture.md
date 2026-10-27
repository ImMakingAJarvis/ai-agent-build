# Wake-Word Test Error + HA Light Trigger Capture

**Date:** 2026-10-26 (all times PDT)
**Job:** chat-1793010480-2f27 — capture the exact TTS error text the Sonos spoke
and the exact HA service call that changed lights (entity, color, trigger source)
**Sources:** Home Assistant 2026.9.4 logbook + current states (111.1.2.200:80,
REST + websocket) and the `jarvis-voice` journal on this box. Follow-up to
`phase4-voice-ha-log-capture.md` (17:12), which captured the logbook through
16:04; this round re-pulled it through 18:05 and adds the 16:05–18:05 window.
No code changes.

## Answers

**The last 2 test events:** **17:35:46** and **17:56:25** — the two newest VPE
wake reactions in the logbook. Both were *ack-only*: the satellite went
`listening` (~12 s) and straight back to `idle` without ever reaching
`processing` or `responding`. In both windows there was **no TTS synthesis, no
conversation-agent call, no Sonos state change, and no light change**. The
pipeline died at the wake, exactly matching the dead-`stt.speech_to_phrase`
root cause from the 16:45 diagnosis. (17:48:02–10: an HA restart wave sits
between the two tests.)

**The exact TTS error text the Sonos spoke: not recoverable from this box, and
per the HA data no Sonos actually spoke any TTS today.**
- Today's exhaustive TTS synthesis list (a `tts.*` state change = one
  synthesis; every other `tts.*` logbook line is a restart state-restore):
  01:29:22/38/57 `tts.nathan_s_iphone` (×3), 02:46:50 `tts.piper`,
  **03:24:28 `tts.google_translate_en_com`**, 16:11:54 `tts.piper`,
  16:30:31 `tts.google_translate_en_com`.
- No Sonos media-player event correlates with any of them. At 16:11:55–16:12:11
  and 16:30:32–39 the *VPE* speaker played (16 s / 7 s bursts). At 03:24:28
  **no Sonos speaker was even online**: Sonos Roam was off (its first logbook
  entry of the day is 12:51:56), Sonos Move was `unavailable`, and Sonos Living
  Room was `unavailable` 02:53–03:54. So the 03:24:28 synthesis could not have
  reached a Sonos; the VPE had only a 1 s "playing" burst at 03:24:30–31 (too
  short for a sentence — the known playback defect).
- The 03:24:28 synthesis is the only candidate "error text" (it happened while
  the satellite was `responding`, 2 s after `processing` — too fast for an
  STT+LLM round trip, i.e. consistent with the pipeline's STT-failure
  `stt_error` message). Its exact text is **not in any API-reachable log**: the
  TTS entity stores only the synthesis timestamp, and the text lives in
  `home-assistant.log` / the pipeline config on the HA box, which has no SSH
  access from this box.

**The exact HA service call that changed lights:** the last real light change
was **12:27:39**: `light.staircase_foyer_stairs` (Hue bulb "Foyer Stairs") and
`light.staircase_staircase` (Hue "Staircase" room group, member = Foyer Stairs)
→ **on**, same second — i.e. a plain `light.turn_on` with no color/brightness
params: the bulbs have sat at color_temp **6535 K** (the bulb's maximum —
Hue's default cool white), brightness **178/255 (~70%)**, rgb (255,255,251),
effect off, ever since (verified against current states; no change until the
17:48:06 restart restore). **Trigger source: no HA automation, script, or
scene fired** — the entire logbook (00:00–18:05) contains only the two
per-minute camera/timelapse automations. The owner was physically home: four
tracked entities entered `zone.home` at 12:22:46, his MacBook
("Natebook Pro 14", 111.1.2.123) joined the network at 12:26:12, front-door
motion+person ran 12:27:13–12:27:50, back-door person at 12:27:44. So the
source is either an owner tap in the HA app or a Hue-bridge-side trigger (HA
would see only the state change); the logbook carries no context for light
entries, so naming the caller needs `home-assistant.log`.
The earlier whole-house event (03:24:28–29, ~30 lights, same second as the
TTS while the satellite was `responding`) is the same story: our `jarvis_voice`
agent is excluded (zero voice-service requests 03:15–03:30, and no
`conversation.*` entry between 01:25:37 and 16:04:26), so if voice-driven it
was the built-in HA agent or a coincident app action — undecidable from here.

## Evidence

### Last 2 wake tests (full logbook windows)

| 17:35:46 window | 17:56:25 window |
|---|---|
| 17:35:40 person at mom's doorbell | 17:56:22 washer sub-cycle FILL |
| **17:35:46 VPE playing + satellite `listening`** | **17:56:25 VPE playing + satellite `listening`** |
| 17:35:47 VPE idle (1 s) | 17:56:26 VPE idle (1 s) |
| 17:35:58 satellite `idle` | 17:56:38 satellite `idle` |

No `tts.*` synthesis, no `conversation.*`, no `light.*`, no Sonos state change
in either window. Sonos Roam was `paused` 17:24:37 → 17:50:31 → `idle`
18:02:48; its 18:02:52 `playing` is Spotify "Yayu" (Mage) via the Music
Assistant queue — music resuming (g3 motion 18:02:51), not TTS.

### 03:24:28 sequence (last night, round 3)

```
03:24:15 VPE playing + satellite listening      (11 s of listening = speech)
03:24:24 front_door_speaking_detected on
03:24:26 satellite processing
03:24:28 satellite responding
03:24:28 tts.google_translate_en_com synthesis
03:24:28-29 ~30 lights -> on (whole house, no groups light.house/downstairs)
03:24:30-31 VPE playing 1 s
03:24:56-57 VPE playing 1 s + satellite listening (second attempt)
```

Sonos availability at 03:24: Roam off (first entry 12:51:56), Move
`unavailable`, Living Room `unavailable` 02:53–03:54.

### 12:27:39 sequence

```
12:22:46 zone.home 1->4 (four trackers enter)
12:26:12 device_tracker "Natebook Pro 14" (111.1.2.123) -> home
12:27:13 front-door motion/person on
12:27:39 front_door_motion off; light.staircase_foyer_stairs on;
         light.staircase_staircase on        <- the light command
12:27:41-50 front-door motion re-fires
12:27:44-46 back-door person + motion
```

Color (current = since 12:27:39): color_temp 6535 K, brightness 178/255,
rgb (255,255,251), no effect.

## Limitations

- No recorder addon on this HA: no state history, no service-call history, no
  pipeline config via API (all 404/unknown). Logbook entries for `light.*`
  carry no context; only `event.*` and some `conversation.*` rows do.
- No SSH/Supervisor access to the HA box: `home-assistant.log` (which would
  name the `light.turn_on` caller and carry the TTS text) is unreadable from
  here.
- Spoken utterances are in no log (dead STT; voice service doesn't log
  inbound text) — unchanged from the 17:12 capture.

## Open / follow-ups

1. SSH into the HA box (or Supervisor terminal) to read
   `home-assistant.log`: names the 12:27:39 and 03:24:28 `light.turn_on`
   callers and the 03:24:28 TTS text (pipeline `stt_error` string / agent
   reply), and the "Jarvis" pipeline's agent/STT/TTS config.
2. Fix `stt.speech_to_phrase` (16:45 diagnosis) — 17:35/17:56 show it's still
   dead: both tests stop at satellite `listening`.
3. Log inbound text in `/voice/json` (one `logger.info`) so the next capture
   shows what the pipeline heard.
