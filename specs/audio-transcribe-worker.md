---
id: audio-transcribe-worker
name: audio.transcribe worker (Whisper, CPU)
summary: Transcribes audio clips on the CPU with faster-whisper, alongside the brain.
group: capability
kind: container
status: active
compose: [audio-transcribe-worker]
binaries: [cmd/audio-transcribe-worker]
ports: ["— (no listener)"]
docs: [docs/benchmarks/audio-transcribe-whisper-benchmark.md, docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes cap.audio.transcribe"
  - to: resourced
    why: "RAM lease per consume session"
---

## Purpose

EVB-22. faster-whisper small (int8) on the CPU — chosen over whisper.cpp
at 3.6× the speed and equal or better WER. No GPU, so it never needs a
window. Per-clip JSON + txt + index.json in the Result.

## Interfaces

- Consumes `cap.audio.transcribe`; input refs are audio files.

## Configuration

- `AT_THREADS`, `RABBITMQ_URL`, `RESOURCED_URL`, `CAPABILITIES_FILE`.
- Models from `/opt/jarvis-whisper` (ro).

## Data

- Results under the request's out dir.

## Deploy

    docker compose build audio-transcribe-worker && docker compose up -d audio-transcribe-worker

## Verify live

Submit a known clip and compare the transcript:

    jarvis-mq submit audio.transcribe --input-ref <clip.wav> --reply-to flow.check-$(date +%s) --corr t1

## Failure modes

- Without the `av.open` kwarg fix in the image, requests dead-lettered
  (2026-11-10).
