"""One-shot faster-whisper transcription for the audio-transcribe worker.

Usage: transcribe.py <input> <out.json> <model_dir> [threads] [beam_size]

Loads the CT2 Whisper model from model_dir (CPU, int8), transcribes
<input> (any container PyAV decodes — mp4/mov/mkv/wav/mp3/m4a/...), and
writes the result as JSON to <out.json>:

  {file, language, language_probability, duration,
   segments: [{start, end, text}], text,
   model_load_seconds, transcribe_seconds, threads, beam_size}

The model is loaded per invocation (~2.2 s for small) and the process
exits when done — the worker keeps the process short-lived on purpose:
no coprocess daemon, no idle RAM hold (docs/benchmarks/
audio-transcribe-whisper-benchmark.md).
"""
import json
import sys
import time


def main():
    if len(sys.argv) < 4:
        sys.stderr.write(__doc__.splitlines()[1] + "\n")
        return 2
    inp, out, model_dir = sys.argv[1], sys.argv[2], sys.argv[3]
    threads = int(sys.argv[4]) if len(sys.argv) > 4 else 0
    beam = int(sys.argv[5]) if len(sys.argv) > 5 else 5

    t0 = time.time()
    from faster_whisper import WhisperModel

    model = WhisperModel(model_dir, device="cpu", compute_type="int8",
                         cpu_threads=threads or None)
    t_load = time.time() - t0
    sys.stderr.write(f"transcribe: model loaded in {t_load:.2f}s ({model_dir})\n")

    t1 = time.time()
    segments, info = model.transcribe(inp, beam_size=beam, vad_filter=False)
    segs = [{"start": round(s.start, 3), "end": round(s.end, 3), "text": s.text.strip()}
            for s in segments]
    t_dec = time.time() - t1
    sys.stderr.write(
        f"transcribe: {len(segs)} segment(s), {info.duration:.1f}s audio "
        f"({info.language}) in {t_dec:.2f}s\n")

    result = {
        "file": inp,
        "language": info.language,
        "language_probability": round(info.language_probability, 4),
        "duration": round(info.duration, 3),
        "segments": segs,
        "text": " ".join(s["text"] for s in segs),
        "model_load_seconds": round(t_load, 3),
        "transcribe_seconds": round(t_dec, 3),
        "threads": threads,
        "beam_size": beam,
    }
    with open(out, "w") as f:
        json.dump(result, f, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
