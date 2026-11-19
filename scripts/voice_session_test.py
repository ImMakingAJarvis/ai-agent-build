#!/usr/bin/env python3
"""
voice_session_test.py — end-to-end test run of the voice call (docs/voice-call.md).

Runs INSIDE the voice container, where it can reach the service on
127.0.0.1:8100, the engines by compose name (engine:11434,
engine-voice:11436), and edge-tts for the synthetic speech:

    docker exec jarvis-voice python3 /opt/jarvis-voice/scripts/voice_session_test.py

Steps:
  0. If a call is already active (a previous run died mid-call), end it
     and wait for idle + window closed.
  1. Say "Okay, talk to me." (synthesized) via POST /voice. Expect the
     canned opening reply and state=opening.
  2. Poll GET /voice/session until state=active + window=open (the swap
     window: brain out, call engine up) — up to 6 minutes.
  3. One normal turn via POST /voice -> expect a WAV reply (the call
     engine answered, not the brain).
  4. One quiet turn (attenuated to ~-45 dBFS, below the -40 gate) ->
     expect {"status": "ignored"} (background noise, no reply).
  5. Say "Okay, thanks, bye." -> expect the canned goodbye and state=idle.
  6. Poll until window=closed and the brain engine (compose name
     engine:11434) is healthy again — up to 6 minutes.

Prints one line per step with timestamps; the last line is "PASS ..." or
"FAIL: ..." and the exit code is 0/1.
"""

import asyncio
import io
import json
import os
import sys
import time

import edge_tts
import httpx
import numpy as np
import soundfile as sf

BASE = os.getenv("VOICE_TEST_BASE", "http://127.0.0.1:8100")
BRAIN_URL = os.getenv("VOICE_TEST_BRAIN", "http://engine:11434")
USER = os.getenv("VOICE_USER", "")
PASS = os.getenv("VOICE_PASS", "")
TTS_VOICE = os.getenv("TTS_VOICE", "en-US-GuyNeural")

OPEN_TIMEOUT_S = int(os.getenv("VOICE_TEST_OPEN_TIMEOUT_S", "360"))
CLOSE_TIMEOUT_S = int(os.getenv("VOICE_TEST_CLOSE_TIMEOUT_S", "360"))

OPENING_TEXT = "Okay, talk to me."
TURN_TEXT = "What time is it right now?"
QUIET_TEXT = "The weather looks okay for a walk later today."
GOODBYE_TEXT = "Okay, thanks, bye."


def log(msg: str) -> None:
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def auth():
    return (USER, PASS) if USER else None


async def synth_wav(text: str, gain_db: float = 0.0) -> bytes:
    """Synthesize text with edge-tts, return a 16-bit PCM WAV.

    gain_db attenuates the result (negative = quieter) — used for the
    background-noise turn that must fall below the addressed gate.
    """
    communicate = edge_tts.Communicate(text, TTS_VOICE)
    buf = io.BytesIO()
    async for chunk in communicate.stream():
        if chunk["type"] == "audio":
            buf.write(chunk["data"])
    buf.seek(0)
    data, sr = sf.read(buf, dtype="float64")
    if gain_db:
        data = np.clip(data * (10.0 ** (gain_db / 20.0)), -1.0, 1.0)
    out = io.BytesIO()
    sf.write(out, data, sr, format="WAV", subtype="PCM_16")
    out.seek(0)
    return out.read()


async def post_voice(client, wav: bytes):
    return await client.post(
        f"{BASE}/voice",
        files={"file": ("test.wav", wav, "audio/wav")},
        timeout=200.0,
    )


async def get_session(client):
    r = await client.get(f"{BASE}/voice/session", timeout=10.0)
    r.raise_for_status()
    return r.json()


def is_audio(r) -> bool:
    return r.headers.get("content-type", "").startswith("audio")


async def main() -> int:
    failures = []
    async with httpx.AsyncClient(auth=auth()) as client:
        # -- 0. clean slate: end a call left open by a previous run ---------
        s = await get_session(client)
        log(f"Initial session: {json.dumps(s)}")
        if s.get("state") != "idle" or s.get("window") not in (None, "closed"):
            log("Call already in flight — force-ending before the test")
            await client.post(f"{BASE}/voice/session/force-end", timeout=10.0)
            deadline = time.monotonic() + CLOSE_TIMEOUT_S
            while time.monotonic() < deadline:
                s = await get_session(client)
                if s.get("state") == "idle" and s.get("window") in (None, "closed"):
                    break
                await asyncio.sleep(5)
            else:
                failures.append(f"clean-slate: still not idle/closed: {s}")

        # -- 1. trigger ------------------------------------------------------
        log("Trigger: " + OPENING_TEXT)
        r = await post_voice(client, await synth_wav(OPENING_TEXT))
        if r.status_code != 200:
            failures.append(f"trigger: HTTP {r.status_code} {r.text[:200]}")
        else:
            log(f"Trigger: X-Transcript={r.headers.get('X-Transcript')!r} "
                f"X-Reply={r.headers.get('X-Reply')!r}")
        s = await get_session(client)
        log(f"Session after trigger: {json.dumps(s)}")
        if s.get("state") not in ("opening", "active"):
            failures.append(
                f"trigger: state={s.get('state')!r} (want opening/active) — "
                "whisper likely misheard the trigger phrase"
            )

        if not failures:
            # -- 2. wait for the window: state=active + window=open ----------
            log(f"Waiting for the swap window (<= {OPEN_TIMEOUT_S}s)...")
            ok = False
            s = {}
            deadline = time.monotonic() + OPEN_TIMEOUT_S
            while time.monotonic() < deadline:
                s = await get_session(client)
                if s.get("state") == "active" and s.get("window") == "open":
                    ok = True
                    break
                if s.get("state") == "idle":
                    break  # the call ended on its own
                await asyncio.sleep(5)
            if not ok:
                failures.append(
                    f"window: not active+open within {OPEN_TIMEOUT_S}s: {s}"
                    + (f" (window_error: {s.get('window_error')})"
                       if s.get("window_error") else "")
                )
            else:
                log(f"Window open, call active: {json.dumps(s)}")

            if not failures:
                # -- 3. one normal turn --------------------------------------
                log("Turn: " + TURN_TEXT)
                r = await post_voice(client, await synth_wav(TURN_TEXT))
                if r.status_code != 200 or not is_audio(r):
                    failures.append(f"turn: HTTP {r.status_code} {r.text[:200]}")
                else:
                    log(f"Turn reply: {r.headers.get('X-Reply')!r} "
                        f"(X-Session={r.headers.get('X-Session')!r})")

                # -- 4. one quiet turn: background noise, must be ignored ----
                log("Quiet turn (~-45 dBFS): " + QUIET_TEXT)
                r = await post_voice(client, await synth_wav(QUIET_TEXT, gain_db=-45))
                if r.status_code == 400 and "No speech" in r.text:
                    failures.append(
                        "quiet turn: too quiet even for STT — raise the gain "
                        "in voice_session_test.py"
                    )
                elif r.status_code != 200:
                    failures.append(f"quiet turn: HTTP {r.status_code} {r.text[:200]}")
                elif is_audio(r):
                    failures.append(
                        f"quiet turn was ANSWERED (gate failed): "
                        f"X-Reply={r.headers.get('X-Reply')!r}"
                    )
                else:
                    body = r.json()
                    if body.get("status") != "ignored":
                        failures.append(f"quiet turn: expected ignored, got {body}")
                    else:
                        log("Quiet turn ignored as expected")

                # -- 5. goodbye ------------------------------------------------
                log("Goodbye: " + GOODBYE_TEXT)
                r = await post_voice(client, await synth_wav(GOODBYE_TEXT))
                if r.status_code != 200 or not is_audio(r):
                    failures.append(f"goodbye: HTTP {r.status_code} {r.text[:200]}")
                elif "Talk soon" not in (r.headers.get("X-Reply") or ""):
                    failures.append(
                        f"goodbye: unexpected reply {r.headers.get('X-Reply')!r}"
                    )
                else:
                    log(f"Goodbye reply: {r.headers.get('X-Reply')!r} "
                        f"(X-Session={r.headers.get('X-Session')!r})")

        # -- 6. wait for the window to close and the brain to come back ------
        log(f"Waiting for the window to close + brain back (<= {CLOSE_TIMEOUT_S}s)...")
        s = {}
        closed = False
        deadline = time.monotonic() + CLOSE_TIMEOUT_S
        while time.monotonic() < deadline:
            s = await get_session(client)
            if s.get("state") == "idle" and s.get("window") in (None, "closed"):
                closed = True
                break
            await asyncio.sleep(5)
        if not closed:
            failures.append(
                f"close: window not closed within {CLOSE_TIMEOUT_S}s: {s}"
            )
        try:
            br = await client.get(f"{BRAIN_URL}/health", timeout=10.0)
            if br.status_code != 200:
                failures.append(f"brain: /health on {BRAIN_URL} -> {br.status_code}")
            else:
                log("Brain engine healthy again")
        except Exception as e:
            failures.append(f"brain: unreachable at {BRAIN_URL}: {e}")

    if failures:
        log("FAIL:")
        for f in failures:
            log(f"  - {f}")
        return 1
    log("PASS: trigger opened the call, one turn on the call engine, "
        "quiet turn ignored, goodbye closed the window, brain back up")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
