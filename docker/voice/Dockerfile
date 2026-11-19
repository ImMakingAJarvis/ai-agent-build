# Voice service runtime. The code itself is the pinned deploy point
# /opt/jarvis-voice (bind-mounted by docker-compose.yml), matching how the
# native jarvis-voice.service ran it.
# ffmpeg is required by the service itself: ensure_wav() decodes the PWA's
# audio/webm;codecs=opus uploads (and any other non-WAV/FLAC container) and
# prepare_audio()/resample_with_ffmpeg() resamples WAVs that are not already
# 16 kHz mono (2026-11-18: voice 500'd with "No such file or directory:
# 'ffmpeg'" because the slim image shipped without it).
FROM python:3.14-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ffmpeg \
 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir \
    fastapi "uvicorn[standard]" faster-whisper edge-tts \
    soundfile numpy httpx pydantic python-multipart
USER 1001:1001
WORKDIR /opt/jarvis-voice
# No ENTRYPOINT: docker-compose.yml passes the full command, so an entrypoint
# here would duplicate the script path as an extra argument.
