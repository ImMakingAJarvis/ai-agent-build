# voice-latency-map — the reply waits for the whole reply: non-streaming LLM + double TTS are the cuts

2026-11-20, think job. Traced the live voice path (docs/voice-call.md,
`scripts/voice_service.py`, `integrations/jarvis_voice/conversation.py`,
`internal/wake`) and measured the current stack live, because
`docs/wake-latency-measurements.md` (2026-10-21) is stale — it measured
Ollama + base.en + espeak-ng, none of which is in the current path.

## The path as it actually runs

Two live input paths into `jarvis-voice` (:8100), plus the voice-call mode:

- **Satellite (VPE via HA):** wake + STT happen **on the satellite/HA side**;
  the `jarvis_voice` integration POSTs **text** to `/voice/json`
  (`conversation.py:59`, 120 s client timeout). The service runs LLM +
  edge-tts and returns a WAV — **which the integration throws away**: it
  reads the `X-Reply` header and does `async_set_speech()`, i.e. **HA runs a
  second TTS** (HA has local `tts.piper`).
- **PWA / tests (audio):** clip → `POST /voice` → `ensure_wav` (ffmpeg
  subprocess for non-WAV uploads, ~50–150 ms) → faster-whisper **small int8
  on CPU** → LLM → edge-tts → WAV.
- **Voice-call mode** (docs/voice-call.md): same per-turn path, but the
  trigger first opens the VRAM window (**1–3 min one-time**, 27B load into
  `engine-voice` :11436) and turns then pin `base_url` to it.

## Measured (2026-11-20, live)

| stage | number | how |
|---|---|---|
| LLM brain 27B, ~800-tok prompt, **cold** | TTFT 3.2–3.8 s, decode 46 tok/s | live probe, `stream:true`, measured while my own agent round was running (contention included) |
| LLM brain, **warm turn** (same slot, prefix reuse) | **TTFT 0.29 s** | second call, +30 new tokens — llama.cpp prefix caching already works |
| LLM iGPU fallback (3B, idle iGPU), same prompt | TTFT 6.0 s, 13.3 tok/s | live probe — prompt eval dominates |
| edge-tts (cloud), ~10-word reply | first byte 0.68 s, total 1.15 s | live probe; service waits for the **whole** MP3 before replying |
| whisper small int8 CPU | 0.08 s for a 3 s no-speech clip (VAD); **1.3–1.9 s real speech** | container probe + Nov-18 log deltas |
| service-side full pipeline (audio path, X-Elapsed) | min 3.4 / p50 5.2 / max 12.0 s (n=7, Nov 18–19) | `docker logs jarvis-voice` |
| satellite side (wake, endpointing, device STT, HA routing, HA TTS) | **unmeasured** — the README's ~12 s total minus the service's ~4–5 s ≈ 6–9 s perceived | not visible from this box |

Log deltas from Nov 18 turns show the same shape: brain turn = STT ~1.3–1.9 s
+ LLM 1.2–2.6 s + TTS 0.8–1.6 s; the iGPU fallback took 8.1 s for its LLM
turn; the 12 s turn was a correction-pass double LLM call (the model claimed
it queued a job without calling the tool — rare; both prompt profiles already
carry the guard).

## Where the time goes (typical satellite turn)

Perceived wake → first word ≈ (wake + endpointing + device STT, unmeasured,
~2–4 s) + LAN/HA (~0.2 s) + LLM (0.3 s warm / ~3 s cold) + **edge-tts that is
discarded** (0.7–1.2 s) + HA piper TTS (~0.5–1 s). The service does a cloud
TTS call every satellite turn whose output nobody plays.

## Options, biggest cuts first

1. **Stream the LLM and TTS in sentence chunks.** The payload is
   `stream: False` with `max_tokens: 300` and TTS waits for the complete
   reply — the user waits for the *whole* reply before the first syllable.
   Stream, split on sentence boundaries, start TTS on the first sentence:
   first audio lands after TTFT + ~20–40 tokens (~0.5–1 s of decode) instead
   of full-reply + full-TTS. The 12 s case becomes ~4 s; a typical 5 s turn
   ~3 s. Biggest perceived cut, applies to every path.
2. **Kill the double TTS on the satellite path.** Either the integration
   plays the service's WAV (one TTS, keeps the edge-tts voice) or
   `/voice/json` goes text-in/text-out for HA (saves 0.7–1.2 s per turn and
   removes the cloud dependency from the satellite path — HA already has
   local piper). Small change on one side.
3. **Instrument what is invisible.** `/voice/json` never logs its elapsed at
   all, and no stage is stamped. A 5-line change (log `stt/llm/tts` ms +
   route per turn in both endpoints) makes every future cut verifiable and
   seeds the T11 baseline (T11 "E2E test harness + latency benchmark" is
   backlog, waiting on T7 of the voice-channel epic).
4. **Keep the warm path warm.** Prefix reuse already gives 0.29 s warm TTFT;
   cold starts (restart, voice-window reopen, slot eviction with slots=2)
   cost ~3 s. Trim `MAX_HISTORY` 20 → ~10 turns and keep the prefix
   append-only (it already is) → cold ~2 s.
5. **Whisper on the 4090** for the audio path: ~4 GB free VRAM fits
   small-fp16; ~1.3–1.9 s CPU → ~0.2–0.4 s. Only the PWA/test path benefits
   (the satellite already STTs on-device), so low priority.
6. **(Voice-call mode only)** a smaller call engine (7–8B) would cut the
   1–3 min window to 30–60 s and decode faster — but Nathan chose the 27B
   takeover deliberately; an option to offer, not a recommendation.

Not a cut: the per-turn routing probes (`brain_up`/`fallback`) are TTL-cached;
ffmpeg transcodes are tens of ms.

## Recommendation

Do #3 first (five lines — nothing above is provable without it), then land
#1 + #2 together as one voice-latency PR, with #4 as a one-line follow-up in
the same PR. Expected: satellite wake → first spoken word from ~8–12 s to
~3–5 s, and the satellite path stops paying for a cloud TTS it never plays.
#5/#6 stay options for the voice-channel epic (T11's harness is the right
place to re-measure once the direct channel exists).
