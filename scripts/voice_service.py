#!/usr/bin/env python3
"""
voice_service.py — FastAPI service that turns Home Assistant audio into Jarvis voice replies.

Endpoints:
  POST /voice        Full pipeline: audio file in → WAV audio out (STT → LLM → TTS)
  POST /voice/json   Full pipeline via JSON: {"text": "..."} → WAV audio out
  GET  /voice/chat   Real-time 2-way voice chat web interface (WebSocket)
  POST /stt          Speech-to-Text only: audio file in → {"text": "..."}
  POST /tts          Text-to-Speech only: {"text": "..."} → WAV audio out
  POST /history/reset  Clear conversation memory
  GET  /health       Health check (verifies Ollama + whisper model are ready)
  WS   /voice/ws     WebSocket for real-time 2-way audio communication

Designed to sit between Home Assistant and the local LLM.  Home Assistant
sends an audio clip (wav/ogg/mp3); the service transcribes it with
faster-whisper, sends the text to Ollama's jarvis-brain, synthesizes a
reply with edge-tts, and streams the audio back.

Environment:
  OLLAMA_URL      default http://localhost:11434
  LOCAL_MODEL     default jarvis-brain
  WHISPER_MODEL   default medium  (tiny/base/small/medium/large)
  TTS_VOICE       default en-US-GuyNeural
  TTS_LANG        default en-US
  VOICE_PROMPT    path to system prompt (default ~/.config/jarvis/voice-prompt.txt)
  LISTEN_PORT     default 8100
"""

import asyncio
import io
import json
import logging
import os
import subprocess
import time

import edge_tts
import faster_whisper
import httpx
import numpy as np
import soundfile as sf
import uvicorn

from fastapi import FastAPI, File, UploadFile, Request, HTTPException, WebSocket, WebSocketDisconnect, status
from fastapi.responses import Response, JSONResponse, HTMLResponse
from fastapi.security import HTTPBasic
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
OLLAMA_URL = os.getenv("OLLAMA_URL", "http://localhost:11434")
LOCAL_MODEL = os.getenv("LOCAL_MODEL", "jarvis-brain")
WHISPER_MODEL_NAME = os.getenv("WHISPER_MODEL", "small")
TTS_VOICE = os.getenv("TTS_VOICE", "en-US-GuyNeural")
LISTEN_PORT = int(os.getenv("LISTEN_PORT", "8100"))
VOICE_PROMPT_PATH = os.getenv(
    "VOICE_PROMPT", os.path.expanduser("~/.config/jarvis/voice-prompt.txt")
)
VOICE_USER = os.getenv("VOICE_USER", "")
VOICE_PASS = os.getenv("VOICE_PASS", "")

logging.basicConfig(level=logging.INFO, format="%(asctime)s [voice] %(message)s")
logger = logging.getLogger("voice")

# ---------------------------------------------------------------------------
# Voice system prompt
# ---------------------------------------------------------------------------
def load_voice_prompt() -> str:
    if os.path.exists(VOICE_PROMPT_PATH):
        with open(VOICE_PROMPT_PATH) as f:
            return f.read().strip()
    return (
        "You are Jarvis, Nathan's home AI, speaking to him by voice. "
        "Keep replies brief and conversational — one or two sentences max. "
        "Do not use markdown, code blocks, or lists. "
        "You cannot act on the machine directly; if Nathan asks you to do something, "
        "tell him you'll have the coding worker handle it."
    )

VOICE_SYSTEM_PROMPT = load_voice_prompt()

# ---------------------------------------------------------------------------
# Whisper model (loaded once at startup on GPU)
# ---------------------------------------------------------------------------
whisper_model = None
whisper_ready = False

def load_whisper():
    global whisper_model, whisper_ready
    try:
        logger.info("Loading faster-whisper model: %s (CPU)", WHISPER_MODEL_NAME)
        whisper_model = faster_whisper.WhisperModel(
            model_size_or_path=WHISPER_MODEL_NAME,
            device="cpu",
            compute_type="int8",
        )
        whisper_ready = True
        logger.info("Whisper model loaded: %s on CPU", WHISPER_MODEL_NAME)
    except Exception as e:
        logger.error("Failed to load whisper model: %s", e)

# ---------------------------------------------------------------------------
# Audio helpers
# ---------------------------------------------------------------------------
def resample_with_ffmpeg(data: np.ndarray, sr: int, target_sr: int = 16000) -> np.ndarray:
    """Use ffmpeg binary to resample audio to target_sr mono."""
    in_buf = io.BytesIO()
    sf.write(in_buf, data, sr, format="WAV", subtype="PCM_16")
    in_buf.seek(0)
    proc = subprocess.run(
        [
            "ffmpeg", "-y",
            "-i", "pipe:0",
            "-acodec", "pcm_s16le",
            "-ar", str(target_sr),
            "-ac", "1",
            "-f", "wav",
            "pipe:1",
        ],
        input=in_buf.read(),
        capture_output=True,
        timeout=30,
    )
    if proc.returncode != 0:
        logger.error("ffmpeg resample failed: %s", proc.stderr.decode())
        raise RuntimeError(f"ffmpeg resample failed: {proc.stderr.decode()}")
    out, _ = sf.read(io.BytesIO(proc.stdout), dtype="float32")
    return out

def prepare_audio(data: np.ndarray, sr: int) -> np.ndarray:
    """Normalize audio to 16kHz mono float32 in [-1, 1]."""
    # Mono
    if data.ndim > 1:
        data = data.mean(axis=1)
    # Resample if needed
    if sr != 16000:
        data = resample_with_ffmpeg(data, sr, 16000)
    # Normalize peak
    peak = np.max(np.abs(data))
    if peak > 0:
        data = data / peak * 0.95
    return data

# ---------------------------------------------------------------------------
# STT
# ---------------------------------------------------------------------------
async def transcribe(audio_bytes: bytes) -> str:
    """Transcribe audio bytes to text using faster-whisper."""
    if not whisper_ready:
        raise RuntimeError("Whisper model not loaded")

    audio_buf = io.BytesIO(audio_bytes)
    data, sr = sf.read(audio_buf, dtype="float32")
    data = prepare_audio(data, sr)

    segments, info = whisper_model.transcribe(
        data,
        language="en",
        vad_filter=True,
        vad_parameters={"min_silence_duration_ms": 500},
    )
    text = " ".join(seg.text for seg in segments).strip()
    logger.info("STT [%s] -> %s", info.language, text)
    return text

# ---------------------------------------------------------------------------
# LLM chat via Ollama
# ---------------------------------------------------------------------------
chat_history: list[dict] = []
MAX_HISTORY = 20

async def ask_ollama(user_text: str) -> str:
    """Send text to Ollama and get a reply."""
    messages = [
        {"role": "system", "content": VOICE_SYSTEM_PROMPT},
        *chat_history[-MAX_HISTORY:],
        {"role": "user", "content": user_text},
    ]

    async with httpx.AsyncClient(timeout=120.0) as client:
        resp = await client.post(
            f"{OLLAMA_URL}/v1/chat/completions",
            json={
                "model": LOCAL_MODEL,
                "messages": messages,
                "stream": False,
                "temperature": 0.7,
                # Voice replies are one or two sentences; cap the output.
                "max_tokens": 300,
                # The brain thinks by default; thinking burns the token cap
                # (empty replies) and adds latency. Voice does not need it.
                "chat_template_kwargs": {"enable_thinking": False},
            },
        )
        resp.raise_for_status()
        result = resp.json()
        reply = result.get("choices", [{}])[0].get("message", {}).get(
            "content", "(no reply)"
        )

    chat_history.append({"role": "user", "content": user_text})
    chat_history.append({"role": "assistant", "content": reply})
    logger.info("LLM reply: %s", reply[:100])
    return reply

# ---------------------------------------------------------------------------
# TTS
# ---------------------------------------------------------------------------
async def synthesize(text: str) -> bytes:
    """Synthesize text to speech using edge-tts, return WAV bytes."""
    communicate = edge_tts.Communicate(text, TTS_VOICE)
    audio_bytes = io.BytesIO()
    async for chunk in communicate.stream():
        if chunk["type"] == "audio":
            audio_bytes.write(chunk["data"])
    audio_bytes.seek(0)
    return audio_bytes.read()

def ascii_header(value: str) -> str:
    """Make a string safe for use in an HTTP header (latin-1)."""
    return value.encode("latin-1", "replace").decode("latin-1")

# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------
class TextRequest(BaseModel):
    text: str

# ---------------------------------------------------------------------------
# FastAPI app
# ---------------------------------------------------------------------------
from contextlib import asynccontextmanager

security = HTTPBasic(auto_error=False)

async def require_auth(request: Request):
    """FastAPI dependency that enforces HTTP Basic Auth when credentials are set."""
    if not VOICE_USER or not VOICE_PASS:
        return
    credentials = await security(request)
    if not credentials or credentials.username != VOICE_USER or credentials.password != VOICE_PASS:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid credentials",
            headers={"WWW-Authenticate": "Basic"},
        )

@asynccontextmanager
async def lifespan(context_app):
    load_whisper()
    if VOICE_USER and VOICE_PASS:
        logger.info("Basic auth enabled (user: %s)", VOICE_USER)
    logger.info("Voice service starting on port %d", LISTEN_PORT)
    yield

app = FastAPI(title="Jarvis Voice Service", version="0.1.0", lifespan=lifespan)

# Apply basic auth to all endpoints when credentials are configured
app.dependencies = [require_auth]

@app.get("/health")
async def health():
    """Check that whisper is loaded and the brain is reachable."""
    llm_ok = False
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(f"{OLLAMA_URL}/v1/models")
            if resp.status_code == 200:
                # llama.cpp reports the GGUF path; Ollama reports e.g.
                # "jarvis-brain:latest" — match by substring.
                models = [m.get("name", "")
                          for m in resp.json().get("models", [])]
                llm_ok = any(LOCAL_MODEL in m for m in models) or bool(models)
    except Exception:
        pass

    status = "ok" if (whisper_ready and llm_ok) else "degraded"
    code = 200 if status == "ok" else 503
    return JSONResponse(
        status_code=code,
        content={
            "status": status,
            "whisper": whisper_ready,
            "llm": llm_ok,
            "llm_url": OLLAMA_URL,
            "model": LOCAL_MODEL,
        },
    )

@app.post("/stt")
async def stt_endpoint(file: UploadFile = File(...)):
    """Speech-to-Text: upload audio file, get text back."""
    audio = await file.read()
    try:
        text = await transcribe(audio)
    except Exception as e:
        logger.exception("STT error")
        return JSONResponse(status_code=500, content={"error": str(e)})
    return JSONResponse(content={"text": text})

@app.post("/tts")
async def tts_endpoint(req: TextRequest):
    """Text-to-Speech: send {"text": "..."}, get WAV audio back."""
    try:
        audio = await synthesize(req.text)
    except Exception as e:
        logger.exception("TTS error")
        return JSONResponse(status_code=500, content={"error": str(e)})
    return Response(content=audio, media_type="audio/wav")

@app.post("/voice")
async def voice_endpoint(file: UploadFile = File(...)):
    """Full voice pipeline: audio file in -> WAV audio out."""
    start = time.time()
    audio = await file.read()

    try:
        text = await transcribe(audio)
        if not text:
            return JSONResponse(
                status_code=400,
                content={"error": "No speech detected in audio"},
            )

        reply = await ask_ollama(text)
        reply_audio = await synthesize(reply)

        elapsed = time.time() - start
        logger.info("Voice pipeline completed in %.1fs", elapsed)
        return Response(
            content=reply_audio,
            media_type="audio/wav",
            headers={
                "X-Transcript": ascii_header(text),
                "X-Reply": ascii_header(reply),
                "X-Elapsed": f"{elapsed:.1f}",
            },
        )
    except RuntimeError as e:
        return JSONResponse(status_code=503, content={"error": str(e)})
    except httpx.HTTPStatusError as e:
        return JSONResponse(
            status_code=502,
            content={"error": f"Ollama error: {e.response.text}"},
        )
    except Exception as e:
        logger.exception("Voice pipeline error")
        return JSONResponse(status_code=500, content={"error": str(e)})

@app.post("/voice/json")
async def voice_json_endpoint(req: TextRequest):
    """Full pipeline via JSON body: {"text": "..."} -> WAV audio out."""
    start = time.time()
    try:
        reply = await ask_ollama(req.text)
        reply_audio = await synthesize(reply)
        elapsed = time.time() - start
        return Response(
            content=reply_audio,
            media_type="audio/wav",
            headers={"X-Reply": ascii_header(reply), "X-Elapsed": f"{elapsed:.1f}"},
        )
    except Exception as e:
        logger.exception("Voice JSON pipeline error")
        return JSONResponse(status_code=500, content={"error": str(e)})

@app.post("/history/reset")
async def reset_history():
    """Clear conversation history."""
    chat_history.clear()
    return JSONResponse(content={"status": "history cleared"})

@app.get("/voice/chat")
async def voice_chat_page():
    """Serve the real-time voice chat web interface."""
    from fastapi.responses import FileResponse
    import os
    html_path = os.path.join(os.path.dirname(__file__), "voice_chat.html")
    return FileResponse(html_path)

# ---------------------------------------------------------------------------
# WebSocket endpoint for real-time 2-way audio communication
# ---------------------------------------------------------------------------
# Protocol:
#   Client → Server:
#     - Binary frames: 16-bit PCM audio at 16kHz, mono (chunks of ~200ms = 6400 bytes)
#     - JSON: {"type": "start"}  — begin speaking
#     - JSON: {"type": "end"}    — finished speaking, process the buffer
#   Server → Client:
#     - JSON: {"type": "status", "state": "listening|processing|speaking|idle", "text": "..."}
#     - Binary frames: TTS audio chunks (raw 16-bit PCM at 24kHz, mono)
#     - JSON: {"type": "error", "message": "..."}

async def voice_websocket(ws: WebSocket):
    await ws.accept()

    # Send initial idle state
    await ws.send_json({"type": "status", "state": "idle"})

    audio_buffer = bytearray()
    processing = False

    try:
        while True:
            data = await ws.receive()

            if data.get("type") == "websocket.disconnect":
                break

            # Handle JSON control messages
            if data.get("type") == "websocket.receive" and isinstance(data["text"], str):
                try:
                    msg = json.loads(data["text"])
                except json.JSONDecodeError:
                    continue

                if msg.get("type") == "start":
                    audio_buffer.clear()
                    await ws.send_json({"type": "status", "state": "listening"})

                elif msg.get("type") == "end":
                    if processing:
                        continue  # already processing a previous utterance

                    if len(audio_buffer) < 3200:  # <100ms of audio, ignore
                        await ws.send_json({"type": "status", "state": "idle"})
                        continue

                    processing = True
                    await ws.send_json({"type": "status", "state": "processing"})

                    try:
                        # STT
                        text = await transcribe(bytes(audio_buffer))
                        if not text:
                            await ws.send_json({
                                "type": "status", "state": "idle",
                                "text": "",
                            })
                            processing = False
                            audio_buffer.clear()
                            continue

                        logger.info("WebSocket STT: %s", text)
                        await ws.send_json({
                            "type": "status", "state": "processing",
                            "text": text,
                        })

                        # LLM
                        reply = await ask_ollama(text)

                        # TTS - stream audio chunks as they arrive
                        await ws.send_json({"type": "status", "state": "speaking", "text": reply})
                        communicate = edge_tts.Communicate(reply, TTS_VOICE)

                        # Collect all audio first, then convert to raw PCM and stream
                        audio_bytes = io.BytesIO()
                        async for chunk in communicate.stream():
                            if chunk["type"] == "audio":
                                audio_bytes.write(chunk["data"])

                        audio_bytes.seek(0)
                        tts_wav = audio_bytes.read()

                        # Convert WAV to raw PCM (edge-tts produces 24kHz 16-bit mono)
                        _, tts_pcm = sf.read(io.BytesIO(tts_wav), dtype="int16")
                        raw_pcm = tts_pcm.tobytes()

                        # Stream in chunks of ~200ms (4800 bytes at 24kHz 16-bit)
                        chunk_size = 4800
                        for i in range(0, len(raw_pcm), chunk_size):
                            chunk = raw_pcm[i:i + chunk_size]
                            await ws.send_bytes(chunk)

                        await ws.send_json({"type": "status", "state": "idle", "text": reply})

                    except Exception as e:
                        logger.exception("WebSocket pipeline error")
                        await ws.send_json({"type": "error", "message": str(e)})
                        await ws.send_json({"type": "status", "state": "idle"})

                    finally:
                        processing = False
                        audio_buffer.clear()

            # Handle binary audio frames
            elif data.get("type") == "websocket.receive" and isinstance(data.get("bytes"), bytes):
                audio_buffer.extend(data["bytes"])

    except WebSocketDisconnect:
        logger.info("WebSocket client disconnected")
    except Exception as e:
        logger.exception("WebSocket error")

app.websocket("/voice/ws")(voice_websocket)

# ---------------------------------------------------------------------------
if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=LISTEN_PORT, log_level="info")
