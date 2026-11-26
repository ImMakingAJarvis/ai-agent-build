#!/usr/bin/env python3
"""
voice_stream_test.py — voice pipeline latency + streaming verification.

Runs INSIDE the voice container, where it can reach the service on
127.0.0.1:8100 and edge-tts for the synthetic speech:

    docker exec jarvis-voice python3 /opt/jarvis-voice/scripts/voice_stream_test.py

Checks:
  A. /voice/json REST latency (the HA satellite path).
     - default            -> WAV body; X-Elapsed includes edge-tts synthesis
     - header X-Audio: 0  -> JSON body, no synthesis (the satellite only
       reads X-Reply and speaks the text with HA's own TTS, so the audio
       is dead weight). On the pre-streaming service the header is
       ignored and the body is still WAV — the content-type shows which
       generation of service is running.
  B. /voice/ws streaming (the browser voice chat).
     Sends synthesized speech and measures, from the "end" message:
       TTFA   time to the FIRST binary audio frame
       total  time to the final "idle" status (whole pipeline)
     Old service: every frame arrives in one burst after the full
     pipeline (TTFA ~= total). Streaming service: the first frame lands
     while the reply is still generating (TTFA << total).
  C. run_tool_loop against a local fake SSE server (no real engine):
     - content deltas  -> sentences emitted at boundaries, in order,
       before the stream ends, reconstructing the full reply
     - tool_call delta -> the tool runs, the loop continues and returns
       the plain answer

Prints one line per measurement; last line is "PASS ..." or "FAIL: ..."
and the exit code is 0/1.
"""

import asyncio
import io
import json
import os
import re
import sys
import time

import aiohttp
import aiohttp.web
import edge_tts
import httpx
import numpy as np
import soundfile as sf

BASE = os.getenv("VOICE_TEST_BASE", "http://127.0.0.1:8100")
USER = os.getenv("VOICE_USER", "")
PASS = os.getenv("VOICE_PASS", "")
TTS_VOICE = os.getenv("TTS_VOICE", "en-US-GuyNeural")

# One utterance that should draw a short conversational reply (1-3
# sentences) — long enough for the pipeline to be measurable, short
# enough to keep the test under a minute.
UTTERANCE = "What are the plans for later today?"

TURN_TIMEOUT_S = 180.0


def log(msg: str) -> None:
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def auth():
    return (USER, PASS) if USER else None


async def synth_pcm16k(text: str) -> bytes:
    """Synthesize text with edge-tts, return raw 16kHz 16-bit mono PCM."""
    communicate = edge_tts.Communicate(text, TTS_VOICE)
    buf = io.BytesIO()
    async for chunk in communicate.stream():
        if chunk["type"] == "audio":
            buf.write(chunk["data"])
    buf.seek(0)
    data, sr = sf.read(buf, dtype="float64")
    if sr != 16000:
        # Linear resample via numpy (test speech only, quality is fine)
        n_out = int(len(data) * 16000 / sr)
        x_old = np.linspace(0.0, 1.0, num=len(data), endpoint=False)
        x_new = np.linspace(0.0, 1.0, num=n_out, endpoint=False)
        data = np.interp(x_new, x_old, data)
    pcm = (np.clip(data, -1.0, 1.0) * 32767).astype("<i2")
    return pcm.tobytes()


# ---------------------------------------------------------------------------
# A. REST /voice/json
# ---------------------------------------------------------------------------
async def rest_check(client) -> list:
    failures = []
    r1 = await client.post(
        f"{BASE}/voice/json", json={"text": UTTERANCE}, timeout=TURN_TIMEOUT_S
    )
    ct1 = r1.headers.get("content-type", "")
    el1 = r1.headers.get("X-Elapsed", "?")
    log(f"A1 default: HTTP {r1.status_code} {ct1.split(';')[0]} "
        f"X-Elapsed={el1}s body={len(r1.content)}B "
        f"reply={r1.headers.get('X-Reply', '')[:60]!r}")
    if r1.status_code != 200 or not ct1.startswith("audio"):
        failures.append(f"A1: expected 200 audio body, got {r1.status_code} {ct1}")

    r2 = await client.post(
        f"{BASE}/voice/json", json={"text": UTTERANCE},
        headers={"X-Audio": "0"}, timeout=TURN_TIMEOUT_S,
    )
    ct2 = r2.headers.get("content-type", "")
    el2 = r2.headers.get("X-Elapsed", "?")
    log(f"A2 X-Audio:0: HTTP {r2.status_code} {ct2.split(';')[0]} "
        f"X-Elapsed={el2}s body={len(r2.content)}B "
        f"reply={r2.headers.get('X-Reply', '')[:60]!r}")
    if r2.status_code != 200:
        failures.append(f"A2: expected 200, got {r2.status_code}")
    elif "json" not in ct2:
        log("A2 note: X-Audio: 0 not honored (body still WAV) — "
            "pre-streaming service")
    return failures


# ---------------------------------------------------------------------------
# B. WebSocket /voice/ws
# ---------------------------------------------------------------------------
async def ws_check() -> list:
    failures = []
    pcm = await synth_pcm16k(UTTERANCE)
    headers = None
    if USER:
        import base64
        headers = {"Authorization": "Basic " +
                   base64.b64encode(f"{USER}:{PASS}".encode()).decode()}
    async with aiohttp.ClientSession(headers=headers) as s:
        async with s.ws_connect(f"{BASE}/voice/ws") as ws:
            await ws.send_json({"type": "start", "sampleRate": 16000})
            await asyncio.sleep(0.3)
            step = 6400  # 200ms at 16kHz 16-bit
            for i in range(0, len(pcm), step):
                await ws.send_bytes(pcm[i:i + step])
                await asyncio.sleep(0.02)
            t_end = time.monotonic()
            await ws.send_json({"type": "end"})

            t_first = t_idle = None
            frames = 0
            total_bytes = 0
            reply = ""

            async def drain():
                nonlocal t_first, t_idle, frames, total_bytes, reply
                while True:
                    msg = await ws.receive()
                    if msg.type == aiohttp.WSMsgType.BINARY:
                        if t_first is None:
                            t_first = time.monotonic()
                        frames += 1
                        total_bytes += len(msg.data)
                    elif msg.type == aiohttp.WSMsgType.TEXT:
                        try:
                            j = json.loads(msg.data)
                        except json.JSONDecodeError:
                            continue
                        # The connection opens with an initial idle status
                        # (no "text"); only a turn-end idle carries text.
                        if (j.get("type") == "status"
                                and j.get("state") == "idle"
                                and "text" in j):
                            t_idle = time.monotonic()
                            reply = j.get("text", "")
                            return
                        if j.get("type") == "error":
                            raise RuntimeError(f"WS error: {j.get('message')}")
                    elif msg.type in (aiohttp.WSMsgType.CLOSED,
                                      aiohttp.WSMsgType.CLOSING,
                                      aiohttp.WSMsgType.CLOSE):
                        raise RuntimeError("WebSocket closed before idle")

            try:
                await asyncio.wait_for(drain(), timeout=TURN_TIMEOUT_S)
            except asyncio.TimeoutError:
                failures.append("B: no idle status within timeout")
                log("B FAIL: timeout")
                return failures
            except RuntimeError as e:
                failures.append(f"B: {e}")
                log(f"B FAIL: {e}")
                return failures

            if t_first is None:
                failures.append("B: no audio frames received")
                log("B FAIL: no audio frames")
                return failures
            log(
                f"B ws: TTFA={t_first - t_end:.1f}s total={t_idle - t_end:.1f}s "
                f"frames={frames} audio={total_bytes / (24000 * 2):.1f}s-of-speech "
                f"reply={reply[:60]!r}"
            )
            # Burst detection: the old service sent every frame in one
            # burst, so TTFA ~= total. A streamed service keeps sending
            # frames for a while after the first one.
            if frames >= 2:
                if t_idle - t_first < 1.0:
                    failures.append(
                        "B: frames arrived in one burst "
                        f"(TTFA {t_first - t_end:.1f}s of "
                        f"{t_idle - t_end:.1f}s) — non-streaming "
                        "behavior"
                    )
                else:
                    log(f"B streaming: first frame {t_first - t_end:.1f}s, "
                        f"last ~{t_idle - t_end:.1f}s — audio landed while "
                        "the reply was still generating")
            else:
                log("B note: only one audio frame (single-sentence reply) "
                    "— streaming not measurable on this turn")
    return failures


# ---------------------------------------------------------------------------
# C. run_tool_loop against a local fake SSE server
# ---------------------------------------------------------------------------
FAKE_PORT = 18099
# Three sentences, one token per "chunk": boundaries fall mid-stream.
FAKE_TOKENS = [
    "Sure.", " I", "'m", " fine", " today.", " Plans", "?", " None", " yet",
    " —", " how", " about", " you", "?",
]


def _delta(payload):
    return json.dumps({"choices": [{"delta": payload}]})


async def _fake_sse(request):
    body = await request.json()
    # First round: the model answers with a tool call. Second round
    # (a tool message is in the history): plain content.
    has_tool_result = any(m.get("role") == "tool" for m in body.get("messages", []))
    if has_tool_result:
        payload = [_delta({"content": t}) for t in FAKE_TOKENS]
        payload.append(_delta({}))
    else:
        payload = [
            _delta({"tool_calls": [{
                "index": 0, "id": "call_1", "type": "function",
                "function": {"name": "search_jobs", "arguments": ""},
            }]}),
            _delta({"tool_calls": [{
                "index": 0,
                "function": {"arguments": '{"q": "voice"}'},
            }]}),
            _delta({}),
        ]
    resp = aiohttp.web.StreamResponse(status=200)
    resp.content_type = "text/event-stream"
    await resp.prepare(request)
    for p in payload:
        await resp.write(f"data: {p}\n\n".encode())
        await asyncio.sleep(0.05)
    await resp.write(b"data: [DONE]\n\n")
    await resp.write_eof()
    return resp


async def loop_check() -> list:
    failures = []
    import voice_service as vs

    app = aiohttp.web.Application()
    app.router.add_post("/v1/chat/completions", _fake_sse)
    runner = aiohttp.web.AppRunner(app)
    await runner.setup()
    site = aiohttp.web.TCPSite(runner, "127.0.0.1", FAKE_PORT)
    await site.start()
    try:
        sentences = []
        tool_ran = []

        async def fake_tool(args):
            tool_ran.append(args)
            return "Found 1 job: voice streaming"

        original = vs.TOOL_RUNNERS.get("search_jobs")
        vs.TOOL_RUNNERS["search_jobs"] = fake_tool
        try:
            async def on_sentence(s):
                sentences.append((time.monotonic(), s))

            t0 = time.monotonic()
            async with httpx.AsyncClient(timeout=30.0) as client:
                reply, _ = await vs.run_tool_loop(
                    client, [{"role": "user", "content": "hi"}],
                    on_sentence=on_sentence,
                    base_url=f"http://127.0.0.1:{FAKE_PORT}", model="fake",
                )
            t_end = time.monotonic()
        finally:
            if original is not None:
                vs.TOOL_RUNNERS["search_jobs"] = original
    except TypeError as e:
        # Pre-streaming service: run_tool_loop has no on_sentence yet.
        log(f"C skip: {e}")
        await runner.cleanup()
        return failures

    log(f"C loop: reply={reply!r} tool_ran={bool(tool_ran)} "
        f"sentences={len(sentences)} stream={t_end - t0:.2f}s")
    if not tool_ran:
        failures.append("C: tool call from the stream was not executed")
    expected = "Sure. I'm fine today. Plans? None yet — how about you?"
    if reply != expected:
        failures.append(f"C: reply mismatch: {reply!r} != {expected!r}")
    got = " ".join(s for _, s in sentences)
    if re.sub(r"\s+", " ", got) != re.sub(r"\s+", " ", expected):
        failures.append(f"C: sentences do not reconstruct the reply: {got!r}")
    # Streaming proof: the first sentence must land well before the
    # stream ends (the fake stream takes ~1.5s; a buffered impl would
    # emit everything in the last instant).
    if sentences:
        first_at = sentences[0][0] - t0
        log(f"C first sentence at {first_at:.2f}s of a "
            f"{t_end - t0:.2f}s stream")
        if first_at > 0.75 * (t_end - t0):
            failures.append("C: first sentence emitted only at stream "
                            "end — content was buffered, not streamed")
    await runner.cleanup()
    return failures


async def main() -> int:
    failures = []
    async with httpx.AsyncClient(auth=auth()) as client:
        failures += await rest_check(client)
    failures += await ws_check()
    failures += await loop_check()
    if failures:
        log("FAIL: " + " | ".join(failures))
        return 1
    log("PASS: REST + WS + streaming loop checks")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
