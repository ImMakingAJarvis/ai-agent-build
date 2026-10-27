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
  OLLAMA_URL      LLM base URL, OpenAI-compatible (default http://localhost:11434;
                  works with the llama.cpp jarvis-engine or real Ollama)
  LOCAL_MODEL     default jarvis-brain
  MEMORY_URL      vector-graph job memory (default http://localhost:8101);
                  voice can search all past job history through it
  JOBS_CLI        job queue CLI (default jarvis-jobs); voice schedules
                  jobs through it
  WHISPER_MODEL   default small   (tiny/base/small/medium/large)
  JARVIS_VOICE    persona profile: "jarvis" (default) or "glados"
  VOICE_PROMPT    override: path to system prompt file
  TTS_VOICE       override: edge-tts voice (e.g. en-US-EmmaNeural)
  TTS_RATE        override: edge-tts rate (e.g. -10%)
  TTS_PITCH       override: edge-tts pitch (e.g. -2Hz)
  LISTEN_PORT     default 8100
  TLS_PORT        HTTPS listener for browser clients (default 8443). Browsers
                  only expose the microphone (getUserMedia) in secure contexts,
                  so the /voice/chat UI needs HTTPS on other LAN machines.
                  Enabled when TLS_CERT/TLS_KEY exist (see gen_voice_cert.sh);
                  the plain-HTTP port stays up for the Home Assistant
                  integration, which speaks http://.
  TLS_CERT / TLS_KEY  default ~/.config/jarvis/tls/voice.{crt,key}
"""

import asyncio
import base64
import hmac
import io
import json
import logging
import os
import signal
import subprocess
import time
import wave

import edge_tts
import faster_whisper
import httpx
import numpy as np
import soundfile as sf
import uvicorn

from fastapi import FastAPI, Depends, File, UploadFile, Request, HTTPException, WebSocket, WebSocketDisconnect, status
from starlette.requests import HTTPConnection
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
LISTEN_PORT = int(os.getenv("LISTEN_PORT", "8100"))
VOICE_USER = os.getenv("VOICE_USER", "")
VOICE_PASS = os.getenv("VOICE_PASS", "")

# TLS listener for browser clients (see module docstring). Enabled
# automatically when the cert + key exist; the plain-HTTP port above keeps
# serving the Home Assistant integration, which speaks http://.
TLS_PORT = int(os.getenv("TLS_PORT", "8443"))
TLS_CERT = os.getenv("TLS_CERT", os.path.expanduser("~/.config/jarvis/tls/voice.crt"))
TLS_KEY = os.getenv("TLS_KEY", os.path.expanduser("~/.config/jarvis/tls/voice.key"))

logging.basicConfig(level=logging.INFO, format="%(asctime)s [voice] %(message)s")
logger = logging.getLogger("voice")

# ---------------------------------------------------------------------------
# Voice profiles (JARVIS_VOICE)
# ---------------------------------------------------------------------------
# Each profile pairs a persona prompt with an edge-tts voice. Prompt files
# live in scripts/voice_prompts/ next to this file. Global overrides:
# VOICE_PROMPT (prompt file), TTS_VOICE / TTS_RATE / TTS_PITCH (TTS).
PROMPT_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "voice_prompts")

VOICE_PROFILES = {
    "jarvis": {
        "name": "Jarvis",
        "prompt": "jarvis.txt",
        "tts_voice": "en-US-GuyNeural",
        "tts_rate": "+0%",
        "tts_pitch": "+0Hz",
    },
    "glados": {
        "name": "GLaDOS",
        "prompt": "glados.txt",
        # Flat, dry delivery: a measured news-anchor voice, slowed down and
        # pitched down slightly for the deadpan.
        "tts_voice": "en-US-EmmaNeural",
        "tts_rate": "-10%",
        "tts_pitch": "-2Hz",
    },
}

ACTIVE_PROFILE_KEY = os.getenv("JARVIS_VOICE", "jarvis").strip().lower()
if ACTIVE_PROFILE_KEY not in VOICE_PROFILES:
    logger.warning(
        "Unknown JARVIS_VOICE=%r; falling back to %r",
        os.getenv("JARVIS_VOICE"), "jarvis",
    )
    ACTIVE_PROFILE_KEY = "jarvis"
ACTIVE_PROFILE = VOICE_PROFILES[ACTIVE_PROFILE_KEY]


def resolve_prompt() -> tuple[str, str]:
    """Return (prompt_text, source_path) for the active profile."""
    override = os.getenv("VOICE_PROMPT")
    if override and os.path.exists(override):
        with open(override) as f:
            return f.read().strip(), override
    path = os.path.join(PROMPT_DIR, ACTIVE_PROFILE["prompt"])
    if os.path.exists(path):
        with open(path) as f:
            return f.read().strip(), path
    # Legacy fallback for the stock profile.
    if ACTIVE_PROFILE_KEY == "jarvis":
        legacy = os.path.expanduser("~/.config/jarvis/voice-prompt.txt")
        if os.path.exists(legacy):
            with open(legacy) as f:
                return f.read().strip(), legacy
    raise RuntimeError(
        f"No system prompt for voice {ACTIVE_PROFILE_KEY!r} in {PROMPT_DIR}"
    )


def tts_settings() -> tuple[str, str, str]:
    """Return (voice, rate, pitch); env overrides beat the profile."""
    return (
        os.getenv("TTS_VOICE") or ACTIVE_PROFILE["tts_voice"],
        os.getenv("TTS_RATE") or ACTIVE_PROFILE["tts_rate"],
        os.getenv("TTS_PITCH") or ACTIVE_PROFILE["tts_pitch"],
    )


VOICE_SYSTEM_PROMPT, VOICE_PROMPT_SOURCE = resolve_prompt()
logger.info(
    "Voice profile: %s (key %s), prompt from %s, TTS %s",
    ACTIVE_PROFILE["name"], ACTIVE_PROFILE_KEY,
    VOICE_PROMPT_SOURCE, tts_settings()[0],
)

# ---------------------------------------------------------------------------
# Whisper model (loaded once at startup on GPU)
# ---------------------------------------------------------------------------
whisper_model = None
whisper_ready = False

def load_whisper():
    """Load the model once (lifespan runs per listener when TLS is enabled)."""
    global whisper_model, whisper_ready
    if whisper_ready:
        return
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

def pcm16_to_wav(pcm: bytes, sample_rate: int) -> bytes:
    """Wrap raw 16-bit mono PCM in a WAV container.

    The WebSocket path accumulates headerless PCM16 frames from the browser;
    transcribe() reads via soundfile, which needs a container header.
    """
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(pcm)
    return buf.getvalue()

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
# LLM chat via the local brain (OpenAI-compatible API) with tools
# ---------------------------------------------------------------------------
chat_history: list[dict] = []
MAX_HISTORY = 20

# Memory-graph service: vector DB over every past job (spec, transcript,
# output), the pending queue, and a snapshot of the repo docs.
MEMORY_URL = os.getenv("MEMORY_URL", "http://localhost:8101")
# Job queue CLI used to schedule work from voice.
JOBS_CLI = os.getenv("JOBS_CLI", "jarvis-jobs")
# Cap the tool loop so a chatty model can't spin forever.
MAX_TOOL_ROUNDS = 3
# Tool results are fed back into the model's context; keep them short.
SEARCH_RESULT_CAP = 2400

# The two abilities voice has on the machine. The brain picks them; the
# service executes them.
VOICE_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "schedule_job",
            "description": (
                "Schedule a job for the coding worker. Use it whenever Nathan "
                "asks for something built, fixed, installed, or changed, or asks "
                "to queue/schedule something. The worker picks it up on its own. "
                "Returns the job id."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "title": {
                        "type": "string",
                        "description": "Short job title, one line.",
                    },
                    "instructions": {
                        "type": "string",
                        "description": "The full work to do, with every detail "
                        "Nathan gave.",
                    },
                    "class": {
                        "type": "string",
                        "enum": ["fix", "build", "background"],
                        "description": "fix for urgent repairs, build for new "
                        "work, background for low-priority. Default build.",
                    },
                },
                "required": ["title", "instructions"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "search_jobs",
            "description": (
                "Search all past work: every job's spec, transcript and output, "
                "plus the pending queue and repo docs. Use it when Nathan asks "
                "what was done about something, what happened in a job, or "
                "whether something is queued. Returns the top matching excerpts."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "What to search for, in plain words.",
                    },
                    "k": {
                        "type": "integer",
                        "description": "Hits to return (default 5, max 8).",
                    },
                },
                "required": ["query"],
            },
        },
    },
]

# UI-facing status for each tool while it runs.
TOOL_STATUS = {
    "schedule_job": "Scheduling job in the queue…",
    "search_jobs": "Searching job history…",
}

async def tool_schedule_job(args: dict) -> str:
    """Run `jarvis-jobs add` for the requested work."""
    title = str(args.get("title") or "").strip()[:200]
    instructions = str(args.get("instructions") or "").strip()[:4000]
    cls = str(args.get("class") or "build").strip().lower()
    if cls not in ("fix", "build", "background"):
        cls = "build"
    if not title or not instructions:
        return "Error: schedule_job needs both a title and instructions."
    job_id = f"voice-{int(time.time())}-{os.urandom(2).hex()}"
    proc = await asyncio.to_thread(
        subprocess.run,
        [
            JOBS_CLI, "add",
            "-id", job_id,
            "-title", title,
            "-instructions", instructions,
            "-class", cls,
            "-source", "voice",
        ],
        capture_output=True, text=True, timeout=30,
    )
    out = (proc.stdout or "").strip()
    if proc.returncode != 0:
        logger.error("schedule_job failed: %s %s", out, proc.stderr)
        return f"Error: could not schedule the job: {proc.stderr.strip()[:200]}"
    if out == "exists":
        return f"The job already exists in the queue as {job_id}."
    logger.info("schedule_job: queued %s (%r) class=%s", job_id, title[:80], cls)
    return f"Scheduled job {job_id} ({cls} class) in the build queue: {title}"

async def tool_search_jobs(args: dict) -> str:
    """Query the memory-graph service and compact the hits for the model."""
    query = str(args.get("query") or "").strip()[:300]
    if not query:
        return "Error: search_jobs needs a query."
    try:
        k = int(args.get("k") or 5)
    except (TypeError, ValueError):
        k = 5
    k = max(1, min(k, 8))
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            resp = await client.post(
                f"{MEMORY_URL}/search",
                json={"query": query, "k": k},
            )
            resp.raise_for_status()
            results = resp.json().get("results", [])
    except Exception as e:
        logger.error("search_jobs failed: %s", e)
        return "Error: the job memory service is unavailable right now."
    if not results:
        return f"No matches for {query!r} in the job history or queue."
    lines = []
    total = 0
    for r in results:
        text = " ".join(str(r.get("text", "")).split())[:280]
        line = (
            f"- [{r.get('source', '?')}] {r.get('title', '')} "
            f"(score {r.get('score', 0):.2f}): {text}"
        )
        if total + len(line) > SEARCH_RESULT_CAP:
            lines.append("- (more hits truncated)")
            break
        lines.append(line)
        total += len(line)
    return f"Top hits for {query!r}:\n" + "\n".join(lines)

TOOL_RUNNERS = {
    "schedule_job": tool_schedule_job,
    "search_jobs": tool_search_jobs,
}

async def ask_brain(user_text: str, on_action=None) -> str:
    """Send text to the brain and get a spoken reply.

    The brain may call the tools in VOICE_TOOLS (schedule_job, search_jobs);
    this runs the tool loop: on tool_calls, execute each one, feed the
    results back, and let the model answer. on_action, if given, is awaited
    with a short status string when a tool starts, so the UI can show what
    voice is doing.
    """
    messages = [
        {"role": "system", "content": VOICE_SYSTEM_PROMPT},
        *chat_history[-MAX_HISTORY:],
        {"role": "user", "content": user_text},
    ]

    async with httpx.AsyncClient(timeout=180.0) as client:
        reply = "(no reply)"
        tools_supported = True
        for round in range(MAX_TOOL_ROUNDS + 1):
            payload = {
                "model": LOCAL_MODEL,
                "messages": messages,
                "stream": False,
                "temperature": 0.7,
                # Voice replies are one or two sentences; cap the output.
                "max_tokens": 300,
                # The brain thinks by default; thinking burns the token cap
                # (empty replies) and adds latency. Voice does not need it.
                "chat_template_kwargs": {"enable_thinking": False},
            }
            # The final round goes out without tools so the model must answer.
            if tools_supported and round < MAX_TOOL_ROUNDS:
                payload["tools"] = VOICE_TOOLS
            try:
                resp = await client.post(
                    f"{OLLAMA_URL}/v1/chat/completions", json=payload
                )
                resp.raise_for_status()
            except httpx.HTTPStatusError as e:
                # An engine without tool support rejects the request; retry
                # the conversation plain rather than fail the utterance.
                if e.response.status_code == 400 and tools_supported:
                    logger.warning(
                        "Engine rejected tools payload (%s); falling back to "
                        "plain chat", e.response.text[:200]
                    )
                    tools_supported = False
                    continue
                raise
            result = resp.json()
            msg = result.get("choices", [{}])[0].get("message", {})
            tool_calls = msg.get("tool_calls") or []
            if not tool_calls:
                reply = msg.get("content") or "(no reply)"
                break
            messages.append({
                "role": "assistant",
                "content": msg.get("content") or None,
                "tool_calls": tool_calls,
            })
            for i, tc in enumerate(tool_calls):
                fn = tc.get("function", {})
                name = fn.get("name", "")
                try:
                    args = json.loads(fn.get("arguments") or "{}")
                except json.JSONDecodeError:
                    args = {}
                if on_action:
                    await on_action(TOOL_STATUS.get(name, f"Working: {name}…"))
                runner = TOOL_RUNNERS.get(name)
                if runner is None:
                    tool_result = f"Error: unknown tool {name!r}."
                else:
                    try:
                        tool_result = await runner(args)
                    except Exception as e:
                        logger.exception("tool %s failed", name)
                        tool_result = f"Error: {name} failed: {e}"
                logger.info("tool %s -> %s", name, tool_result[:120])
                messages.append({
                    "role": "tool",
                    "tool_call_id": tc.get("id") or f"call_{round}_{i}",
                    "name": name,
                    "content": tool_result,
                })
        else:
            # Loop exhausted (model kept calling tools); ask for the answer.
            logger.warning("Tool loop exhausted after %d rounds", MAX_TOOL_ROUNDS)

    chat_history.append({"role": "user", "content": user_text})
    chat_history.append({"role": "assistant", "content": reply})
    logger.info("LLM reply: %s", reply[:100])
    return reply


async def ask_ollama(user_text: str) -> str:
    """Plain (no-tools) chat, kept for callers that don't want the tools."""
    return await ask_brain(user_text)

# ---------------------------------------------------------------------------
# TTS
# ---------------------------------------------------------------------------
async def synthesize(text: str) -> bytes:
    """Synthesize text to speech using edge-tts, return audio bytes (MP3)."""
    voice, rate, pitch = tts_settings()
    communicate = edge_tts.Communicate(text, voice, rate=rate, pitch=pitch)
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

async def require_auth(request: HTTPConnection):
    """FastAPI dependency that enforces HTTP Basic Auth when credentials are set.

    Applied app-wide (see FastAPI(..., dependencies=...) below). Special cases:
    - /voice/chat serves the static web-chat page, which the browser must be
      able to load without a credentials prompt.
    - WebSockets are handled in voice_websocket() itself: browsers cannot set
      the Authorization header on a socket upgrade, and rejecting during the
      dependency phase surfaces as a 403 handshake instead of a clean
      close(1008) the client can react to.
    (HTTPConnection is the common base of Request/WebSocket so one dependency
    can be shared across HTTP and WS routes.)
    """
    if not VOICE_USER or not VOICE_PASS:
        return
    if isinstance(request, WebSocket):
        return
    if request.url.path == "/voice/chat":
        return
    credentials = await security(request)
    if not credentials or credentials.username != VOICE_USER or credentials.password != VOICE_PASS:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid credentials",
            headers={"WWW-Authenticate": "Basic"},
        )

def _ws_token_ok(ws: WebSocket) -> bool:
    """Check the WebSocket credential.

    Browsers cannot set the Authorization header on a socket upgrade, so the
    password is accepted as a ?token= query param (voice_chat.html prompts for
    it); programmatic clients may send a standard Basic Authorization header.
    """
    if not VOICE_USER or not VOICE_PASS:
        return True
    token = ws.query_params.get("token", "")
    header = ws.headers.get("authorization", "")
    if header.lower().startswith("basic "):
        try:
            user, _, password = base64.b64decode(header[6:]).decode().partition(":")
            token = password if user == VOICE_USER else ""
        except Exception:
            token = ""
    return hmac.compare_digest(token, VOICE_PASS)

@asynccontextmanager
async def lifespan(context_app):
    load_whisper()
    if VOICE_USER and VOICE_PASS:
        logger.info("Basic auth enabled (user: %s)", VOICE_USER)
    logger.info("Voice service starting on port %d", LISTEN_PORT)
    yield

# NOTE: app-level dependencies must be passed at construction time; assigning
# app.dependencies afterwards is silently ignored by FastAPI.
app = FastAPI(
    title="Jarvis Voice Service",
    version="0.1.0",
    lifespan=lifespan,
    dependencies=[Depends(require_auth)],
)

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

    # Memory-graph (job history) is a soft dependency: voice still works if
    # it is down, search_jobs just reports it as unavailable.
    memory_ok = False
    try:
        async with httpx.AsyncClient(timeout=3.0) as client:
            resp = await client.get(f"{MEMORY_URL}/health")
            memory_ok = resp.status_code == 200
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
            "memory": memory_ok,
            "memory_url": MEMORY_URL,
            "voice": ACTIVE_PROFILE_KEY,
            "tts_voice": tts_settings()[0],
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
#     - Binary frames: 16-bit PCM audio, mono, at the rate declared in
#       "start" (16kHz on a healthy client; chunks of ~256ms = 8192 bytes)
#     - JSON: {"type": "start", "sampleRate": 16000}  — begin speaking
#     - JSON: {"type": "end"}    — finished speaking, process the buffer
#   Server → Client:
#     - JSON: {"type": "status", "state": "listening|processing|speaking|idle", "text": "..."}
#     - JSON: {"type": "action", "text": "..."}  — a tool is running
#       (scheduling a job / searching job history)
#     - Binary frames: TTS audio chunks (raw 16-bit PCM at 24kHz, mono)
#     - JSON: {"type": "error", "message": "..."}

async def voice_websocket(ws: WebSocket):
    await ws.accept()

    # Enforce auth: browsers can't set the Authorization header on a socket
    # upgrade, so the token arrives as ?token= (or a Basic header from
    # programmatic clients). close(1008) tells the client to re-auth.
    if not _ws_token_ok(ws):
        await ws.close(code=1008)
        return

    # Send initial idle state
    await ws.send_json({"type": "status", "state": "idle"})

    audio_buffer = bytearray()
    processing = False
    declared_rate = 16000  # set from the client's "start" message

    try:
        while True:
            data = await ws.receive()

            if data.get("type") == "websocket.disconnect":
                break

            # Handle JSON control messages
            # (binary frames carry no "text" key — use .get or they raise KeyError)
            if data.get("type") == "websocket.receive" and isinstance(data.get("text"), str):
                try:
                    msg = json.loads(data["text"])
                except json.JSONDecodeError:
                    continue

                if msg.get("type") == "start":
                    audio_buffer.clear()
                    try:
                        declared_rate = int(msg.get("sampleRate") or 16000)
                    except (TypeError, ValueError):
                        declared_rate = 16000
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
                        # STT (the buffer is raw PCM16 — wrap it in a WAV first)
                        text = await transcribe(pcm16_to_wav(bytes(audio_buffer), declared_rate))
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

                        # LLM (with tools: schedule_job / search_jobs).
                        # Tool starts are surfaced as "action" messages so
                        # the UI can show what voice is doing while it works.
                        async def _action(label: str):
                            await ws.send_json({"type": "action", "text": label})

                        reply = await ask_brain(text, on_action=_action)

                        # TTS - stream audio chunks as they arrive
                        await ws.send_json({"type": "status", "state": "speaking", "text": reply})
                        voice, rate, pitch = tts_settings()
                        communicate = edge_tts.Communicate(reply, voice, rate=rate, pitch=pitch)

                        # Collect all audio first, then convert to raw PCM and stream
                        audio_bytes = io.BytesIO()
                        async for chunk in communicate.stream():
                            if chunk["type"] == "audio":
                                audio_bytes.write(chunk["data"])

                        audio_bytes.seek(0)
                        tts_wav = audio_bytes.read()

                        # Convert WAV to raw PCM (edge-tts produces 24kHz 16-bit mono)
                        # sf.read returns (data, samplerate) — data first
                        tts_pcm, _ = sf.read(io.BytesIO(tts_wav), dtype="int16")
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
# Entrypoint
# ---------------------------------------------------------------------------
def tls_enabled() -> bool:
    return os.path.isfile(TLS_CERT) and os.path.isfile(TLS_KEY)


async def serve_all(http_cfg: uvicorn.Config, https_cfg: uvicorn.Config) -> None:
    """Run the plain-HTTP and HTTPS listeners in one process.

    uvicorn's Server.serve() installs its own SIGINT/SIGTERM handlers, so
    two concurrent serve() calls on the main thread would mask each other
    and only one server would ever shut down. Drive the lifecycle manually
    instead: startup() both, wait for a signal ourselves, then main_loop()
    (the should_exit watcher) and shutdown() for each.
    """
    loop = asyncio.get_running_loop()
    servers = []
    try:
        for cfg in (http_cfg, https_cfg):
            cfg.load()
            server = uvicorn.Server(cfg)
            server.lifespan = cfg.lifespan_class(cfg)  # normally set by serve()
            await server.startup()
            servers.append(server)
    except Exception:
        for server in servers:
            server.should_exit = True
            await server.shutdown()
        raise

    stop = loop.create_future()

    def request_stop() -> None:
        if not stop.done():
            stop.set_result(None)

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, request_stop)

    watchers = [asyncio.ensure_future(server.main_loop()) for server in servers]
    await stop
    for server in servers:
        server.should_exit = True
    await asyncio.gather(*watchers)
    for server in servers:
        await server.shutdown()


if __name__ == "__main__":
    if tls_enabled():
        http_cfg = uvicorn.Config(app, host="0.0.0.0", port=LISTEN_PORT, log_level="info")
        https_cfg = uvicorn.Config(
            app,
            host="0.0.0.0",
            port=TLS_PORT,
            ssl_certfile=TLS_CERT,
            ssl_keyfile=TLS_KEY,
            log_level="info",
        )
        logger.info(
            "Voice service starting: http on :%d (Home Assistant), https on :%d (browser UI)",
            LISTEN_PORT, TLS_PORT,
        )
        asyncio.run(serve_all(http_cfg, https_cfg))
    else:
        logger.info("No TLS cert at %s — plain HTTP only on :%d", TLS_CERT, LISTEN_PORT)
        uvicorn.run(app, host="0.0.0.0", port=LISTEN_PORT, log_level="info")
