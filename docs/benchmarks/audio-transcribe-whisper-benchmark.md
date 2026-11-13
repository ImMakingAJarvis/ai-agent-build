# Bench: faster-whisper vs whisper.cpp (audio.transcribe on CPU, EVB-22)

Pick the engine behind `cap.audio.transcribe`: faster-whisper (CTranslate2)
or whisper.cpp, both on CPU, same model (Whisper small), same clip. The
job's acceptance bar is "runs alongside the brain, no window" — so the
decision is wall time, RAM, and quality, with no GPU involvement.

## Setup

- **Machine**: Ryzen 7 7800X3D (8C/16T), 122.7 GB RAM. No GPU used or
  touched (nvidia-smi unchanged during both runs).
- **Clip**: the Gettysburg Address, LibriVox recording (Mark Smith of
  Simpsonville, SC) — 161.23 s, 16 kHz mono WAV, downloaded. Nathan
  approved the downloaded clip for the E2E ("use a downloaded clip for
  now"); a real UniFi camera-clip E2E is pending Protect API / NAS share
  access.
- **Reference**: 325 words, the clip's full speech including the LibriVox
  intro. WER = Levenshtein word distance / 325.
- **Model**: Whisper small. faster-whisper ran the Systran CT2 model
  (int8); whisper.cpp ran an f16 GGUF converted **offline** from that same
  CT2 model (HF egress is blocked on this box — transparent 401 with no
  credentials). Conversion notes for re-runs:

  - The converter (host-local, `/home/agent/evb22-bench/ct2_to_ggml.py`)
    replicates whisper.cpp's own `models/convert-pt-to-ggml.py` byte
    layout (magic, hparams, mel filters, tiktoken vocab, tensors).
  - This build's ggml loader (`src/whisper-arch.h`, master d2c6f05,
    2026-11 "parakeet" era) takes the new gguf-style tensor names —
    `encoder.blocks.{i}.attn.query.weight`, `decoder.blocks.{i}.cross_attn.*`,
    `encoder.ln_post.*`, `decoder.token_embedding.weight` — with **no**
    legacy alias map. The converter emits exactly those; the CT2
    (Marian-style) QKV/KV combined projections are split per the layout
    in the converter's header.
  - In this build `whisper-cli` is `examples/cli/cli.cpp`, and its `-np`
    flag calls `whisper_log_set(cb_log_disable, NULL)` — it suppresses
    every whisper log line, including model-load errors. Run without `-np`
    to see failures.

## Results (161.23 s clip, 16 threads, Whisper small)

| | faster-whisper 1.2.1 (CT2 4.8.3, int8) | whisper.cpp d2c6f05 (f16 GGUF) |
|---|---|---|
| Wall (first transcribe) | **22.98 s** | 81.66 s |
| Wall (second, warm) | **22.48 s** | — |
| RTF | **≈ 0.14** | ≈ 0.51 |
| Peak RSS | **759.7 MB** | 877,172 KB (≈ 856.6 MB) |
| Model load | 2.18 s | 0.175 s |
| WER (325-word ref) | **0.0769** (25 err) | 0.0892 (29 err) |
| Segments | 13 | 18 |

whisper.cpp internals (for the curious): encode 14,280.43 ms over 8 runs
(1,785.05 ms/run) + decode 5,916.97 ms over 37 runs.

## Decision: faster-whisper

3.6× faster on the same clip at equal-or-better WER, and ~100 MB less
peak RAM. whisper.cpp's one advantage is the near-instant model load
(0.18 s vs 2.18 s) — irrelevant at this clip length (a 160 s clip takes
23 s, and camera event clips are shorter, where the 2 s load is a
bigger fraction but still well under the wall time).

The worker therefore runs faster-whisper as a **per-request Python
subprocess** (`cmd/audio-transcribe-worker/transcribe.py`): the model
loads per invocation (~2.2 s), the request transcribes, the process
exits. No coprocess daemon, no resident RAM at idle, nothing to
idle-unload — which is exactly the "alongside the brain, no window"
shape the capability wants.

Artifacts: `results/faster-whisper-small.json` and
`results/wcpp-small.{json,txt,log}` (host-local,
`/home/agent/evb22-bench/results/`); the clip is
`/home/agent/evb22-bench/download/gettysburg-16k.wav`.
