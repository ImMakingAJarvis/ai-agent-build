# audio-transcribe-worker runtime (plan task EVB-22, cap.audio.transcribe).
#
# The worker binary is a static Go build (like docker/jarvis-go); the
# runtime base is python:slim rather than the engine image because the
# engine is faster-whisper (CTranslate2) running as a per-request Python
# subprocess (transcribe.py, shipped below) — there is no llama.cpp
# coprocess to mount, and no GPU at all (the capability runs alongside
# the brain, no window). ctranslate2 and onnxruntime (pulled in by
# faster-whisper's VAD) need the OpenMP runtime the slim image does not
# ship. Versions are pinned to the ones the benchmark measured
# (docs/benchmarks/audio-transcribe-whisper-benchmark.md).
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./${BIN}"

FROM python:3.12-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 \
 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir faster-whisper==1.2.1 ctranslate2==4.8.3 av==19.0.2
# PyPI's faster-whisper 1.2.1 passes av.open(..., metadata_errors="ignore"),
# a kwarg av 19.0.2 (the version the benchmark measured) does not accept —
# the host install carries this same one-line fix, and the container must
# match it byte-for-byte (docs/benchmarks/audio-transcribe-whisper-benchmark.md).
RUN sed -i 's/av\.open(input_file, mode="r", metadata_errors="ignore")/av.open(input_file, mode="r", )/' \
    /usr/local/lib/python3.12/site-packages/faster_whisper/audio.py \
 && grep -q 'av.open(input_file, mode="r", )' /usr/local/lib/python3.12/site-packages/faster_whisper/audio.py
COPY --from=build /out/app /usr/local/bin/app
COPY cmd/audio-transcribe-worker/transcribe.py /usr/local/lib/jarvis-whisper/transcribe.py
# uid/gid of the 'agent' user on the host: mounted host dirs are agent-owned.
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/app"]
