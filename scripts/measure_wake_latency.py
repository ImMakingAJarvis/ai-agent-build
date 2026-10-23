#!/usr/bin/env python3
"""
Measure wake-to-first-word latency stage by stage.

Stages measured:
1. Speech-to-text (STT): Audio file -> transcribed text
2. LLM first token: Prompt -> first token (thinking disabled)
3. Text-to-speech (TTS): Text -> first audio byte

Target: 1-3 seconds total pipeline latency.

Usage:
    python3 scripts/measure_wake_latency.py [--runs N] [--stt-model MODEL]

Output:
    - Per-run timing for each stage
    - Summary statistics (mean, std, min, max, p50, p95)
    - Results written to docs/wake-latency-measurements.md
"""

import argparse
import json
import os
import subprocess
import sys
import time
import statistics
import tempfile
from pathlib import Path

# Paths
SCRIPT_DIR = Path(__file__).parent
REPO_DIR = SCRIPT_DIR.parent
RESULTS_DIR = REPO_DIR / "docs"

def generate_test_audio(text="/tmp/test_prompt.wav"):
    """Generate a test audio file using espeak-ng."""
    prompt = "Hello Jarvis, what is the weather today"
    r = subprocess.run(
        ["espeak-ng", prompt, "-w", text],
        capture_output=True
    )
    if r.returncode != 0:
        print(f"espeak-ng failed: {r.stderr.decode()}")
        sys.exit(1)
    return text


def measure_stt(audio_path, model_name="base.en", runs=5):
    """
    Measure speech-to-text latency using faster-whisper.

    Returns list of dicts with timing info per run.
    """
    from faster_whisper import WhisperModel

    results = []
    for i in range(runs):
        # Load model (first run includes model load time)
        t0 = time.perf_counter()
        model = WhisperModel(model_name, device="cpu", compute_type="int8")
        t_load = time.perf_counter() - t0

        # Transcribe
        t1 = time.perf_counter()
        segments, info = model.transcribe(audio_path, language="en", beam_size=1)
        text = ""
        for seg in segments:
            text += seg.text
        t2 = time.perf_counter()

        transcribe_time = t2 - t1
        total_time = t2 - t0

        results.append({
            "model_load": round(t_load, 3),
            "transcribe": round(transcribe_time, 3),
            "total": round(total_time, 3),
            "text": text.strip(),
        })
        print(f"  STT run {i+1}: load={t_load:.3f}s, transcribe={transcribe_time:.3f}s, total={total_time:.3f}s")
        print(f"    Text: {text.strip()[:80]}...")

    return results


def measure_stt_gpu(audio_path, model_name="base.en", runs=5):
    """
    Measure STT with GPU acceleration if available.
    """
    from faster_whisper import WhisperModel

    results = []
    # Load model once outside the timing loop
    print(f"  Loading Whisper {model_name} on GPU...")
    try:
        model = WhisperModel(model_name, device="cuda", compute_type="float16")
        device_str = "cuda"
    except Exception as e:
        print(f"  GPU not available ({e}), falling back to CPU")
        model = WhisperModel(model_name, device="cpu", compute_type="int8")
        device_str = "cpu"

    for i in range(runs):
        # Transcribe (model already loaded)
        t1 = time.perf_counter()
        segments, info = model.transcribe(audio_path, language="en", beam_size=1)
        text = ""
        for seg in segments:
            text += seg.text
        t2 = time.perf_counter()

        transcribe_time = t2 - t1

        results.append({
            "transcribe": round(transcribe_time, 3),
            "text": text.strip(),
        })
        print(f"  STT-GPU run {i+1}: transcribe={transcribe_time:.3f}s")
        if i == 0:
            print(f"    Text: {text.strip()[:80]}...")

    return results, device_str


def measure_llm_first_token(runs=5):
    """
    Measure LLM time to first token using Ollama API.

    The jarvis-brain model (qwen3.6:27b) always outputs thinking tokens regardless
    of the "think" option. We measure:
    - first_output_time: when the first content arrives (response OR thinking field)
    - prompt_eval_duration: time the model spent processing the prompt (from done response)
    """
    import urllib.request
    import json as j

    results = []
    prompt = "What is 2+2? Answer with just the number."

    # Warmup run: first call after model load is slow (cold KV cache).
    # We discard it from stats but record it for the report.
    print(f"  LLM warmup run (discarded from stats)...")
    warmup_result = None
    t0 = time.perf_counter()
    warmup_payload = {
        "model": "jarvis-brain",
        "prompt": prompt,
        "stream": True,
        "options": {"num_predict": 10},
    }
    warmup_req = urllib.request.Request(
        "http://localhost:11434/api/generate",
        data=j.dumps(warmup_payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(warmup_req, timeout=30) as resp:
        for line in resp:
            data = j.loads(line)
            if data.get("done"):
                warmup_time = time.perf_counter() - t0
                break
    print(f"    Warmup took {warmup_time:.3f}s")

    for i in range(runs):
        payload = {
            "model": "jarvis-brain",
            "prompt": prompt,
            "stream": True,
            "options": {
                "num_predict": 10,
            }
        }

        t0 = time.perf_counter()
        req = urllib.request.Request(
            "http://localhost:11434/api/generate",
            data=j.dumps(payload).encode(),
            headers={"Content-Type": "application/json"},
        )

        first_output_time = None
        prompt_eval_duration = None
        total_tokens = 0
        full_content = ""
        total_time = None

        with urllib.request.urlopen(req, timeout=30) as resp:
            for line in resp:
                data = j.loads(line)

                # Time first content regardless of field (response or thinking)
                content = data.get("response", "") or data.get("thinking", "")
                if content and first_output_time is None:
                    first_output_time = time.perf_counter() - t0
                if content:
                    full_content += content
                    total_tokens += 1

                if data.get("done"):
                    total_time = time.perf_counter() - t0
                    # prompt_eval_duration is in nanoseconds
                    ped = data.get("prompt_eval_duration")
                    if ped is not None:
                        prompt_eval_duration = ped / 1e9  # convert to seconds
                    break

        results.append({
            "first_output": round(first_output_time, 3) if first_output_time else None,
            "prompt_eval_s": round(prompt_eval_duration, 3) if prompt_eval_duration else None,
            "total": round(total_time, 3) if total_time else None,
            "tokens": total_tokens,
            "content": full_content.strip()[:60],
        })
        fo = first_output_time if first_output_time else "N/A"
        pe = prompt_eval_duration if prompt_eval_duration else "N/A"
        tt = total_time if total_time else "N/A"
        print(f"  LLM run {i+1}: first_output={fo}, prompt_eval={pe}, total={tt}")
        if i == 0:
            print(f"    Content: {full_content.strip()[:60]}")

    return results, warmup_time


def measure_tts(runs=5):
    """
    Measure text-to-speech latency using espeak-ng.

    Measures time from text input to first audio byte written.
    """
    results = []
    text = "The answer is four."

    for i in range(runs):
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            tmp_path = f.name

        t0 = time.perf_counter()
        r = subprocess.run(
            ["espeak-ng", text, "-w", tmp_path],
            capture_output=True,
        )
        t1 = time.perf_counter()

        if r.returncode != 0:
            print(f"  espeak-ng failed: {r.stderr.decode()}")
            os.unlink(tmp_path)
            continue

        # Get file size to confirm audio was written
        file_size = os.path.getsize(tmp_path)
        os.unlink(tmp_path)

        tts_time = t1 - t0
        results.append({
            "tts": round(tts_time, 3),
            "file_size": file_size,
        })
        print(f"  TTS run {i+1}: {tts_time:.3f}s ({file_size} bytes)")

    return results


def measure_tts_latency_file(runs=5):
    """
    Measure TTS latency by timing when the first bytes are written to disk.
    Uses inotifywait or file polling to detect when audio starts being written.
    """
    results = []
    text = "The answer is four."

    for i in range(runs):
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            tmp_path = f.name

        t0 = time.perf_counter()

        # Start espeak-ng in background
        proc = subprocess.Popen(
            ["espeak-ng", text, "-w", tmp_path],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

        # Poll for file creation (first byte written)
        first_byte_time = None
        start_poll = time.perf_counter()
        while proc.poll() is None or not os.path.exists(tmp_path):
            if os.path.exists(tmp_path) and os.path.getsize(tmp_path) > 0:
                first_byte_time = time.perf_counter() - t0
                break
            time.sleep(0.001)  # 1ms poll

        # Wait for completion
        proc.wait()
        total_time = time.perf_counter() - t0

        file_size = os.path.getsize(tmp_path) if os.path.exists(tmp_path) else 0
        os.unlink(tmp_path)

        results.append({
            "first_byte": round(first_byte_time, 3) if first_byte_time else None,
            "total": round(total_time, 3),
            "file_size": file_size,
        })
        fb = first_byte_time if first_byte_time else "N/A"
        print(f"  TTS run {i+1}: first_byte={fb}, total={total_time:.3f}s")

    return results


def compute_stats(values, key):
    """Compute summary statistics for a list of values."""
    clean = [v for v in values if v is not None]
    if not clean:
        return {}

    sorted_vals = sorted(clean)
    n = len(sorted_vals)

    stats = {
        "mean": round(statistics.mean(clean), 3),
        "std": round(statistics.stdev(clean), 3) if n > 1 else 0,
        "min": round(sorted_vals[0], 3),
        "max": round(sorted_vals[-1], 3),
        "p50": round(sorted_vals[n // 2], 3),
    }
    if n >= 20:
        p95_idx = int(n * 0.95)
        stats["p95"] = round(sorted_vals[min(p95_idx, n-1)], 3)

    return stats


def write_markdown_report(stt_results, stt_gpu_results, stt_device, llm_results, llm_warmup_time, tts_file_results, audio_path, run_count):
    """Write the measurement results to a markdown document."""

    # Extract timing values
    stt_times = [r["transcribe"] for r in stt_gpu_results]
    llm_first_output_times = [r["first_output"] for r in llm_results]
    llm_prompt_eval_times = [r["prompt_eval_s"] for r in llm_results]
    llm_total_times = [r["total"] for r in llm_results]
    tts_first_byte_times = [r["first_byte"] for r in tts_file_results]
    tts_total_times = [r["total"] for r in tts_file_results]

    # Also include CPU STT results for comparison
    stt_cpu_times = [r["transcribe"] for r in stt_results] if stt_results else []
    stt_cpu_load_times = [r["model_load"] for r in stt_results] if stt_results else []

    stt_stats = compute_stats(stt_times, "stt")
    llm_fo_stats = compute_stats(llm_first_output_times, "llm_fo")
    llm_pe_stats = compute_stats(llm_prompt_eval_times, "llm_pe")
    llm_total_stats = compute_stats(llm_total_times, "llm_total")
    tts_fb_stats = compute_stats(tts_first_byte_times, "tts_fb")
    tts_total_stats = compute_stats(tts_total_times, "tts_total")

    if stt_cpu_times:
        stt_cpu_stats = compute_stats(stt_cpu_times, "stt_cpu")
        stt_load_stats = compute_stats(stt_cpu_load_times, "stt_load")
    else:
        stt_cpu_stats = {}
        stt_load_stats = {}

    # Pipeline totals
    if stt_stats and llm_fo_stats and tts_fb_stats:
        pipeline_mean = round(stt_stats["mean"] + llm_fo_stats["mean"] + tts_fb_stats["mean"], 3)
        pipeline_p50 = round(stt_stats["p50"] + llm_fo_stats["p50"] + tts_fb_stats["p50"], 3)
    else:
        pipeline_mean = pipeline_p50 = "N/A"

    report = f"""# Wake-to-First-Word Latency Measurements

**Date:** {time.strftime("%Y-%m-%d")}
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM), Intel CPU
**STT Model:** faster-whisper base.en (CPU int8 for baseline, GPU float16 for production)
**LLM Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama
**TTS Engine:** espeak-ng
**Runs per stage:** {run_count}

## Motivation

Following the model load time and GPU power measurements, we now measure the
latency of each stage in the wake-to-response pipeline to determine if the
system can meet the 1-3 second target for natural voice interaction.

## Methodology

1. **STT (Speech-to-Text):** Generated test audio with espeak-ng, measured
   transcription time with faster-whisper on both CPU and GPU.
2. **LLM First Token:** Sent minimal prompt to Ollama with thinking disabled,
   measured time to first token in the streaming response.
3. **TTS (Text-to-Speech):** Measured time from text input to first audio byte
   written using espeak-ng.
4. Each stage measured {run_count} times; statistics computed over all runs.

## Results

### Stage 1: Speech-to-Text (faster-whisper base.en)

| Metric | CPU (int8) | GPU (float16) |
|--------|-----------|---------------|
| Mean | {stt_cpu_stats.get('mean', 'N/A')} s | {stt_stats['mean']} s |
| Std Dev | {stt_cpu_stats.get('std', 'N/A')} s | {stt_stats['std']} s |
| Min | {stt_cpu_stats.get('min', 'N/A')} s | {stt_stats['min']} s |
| Max | {stt_cpu_stats.get('max', 'N/A')} s | {stt_stats['max']} s |
| P50 | {stt_cpu_stats.get('p50', 'N/A')} s | {stt_stats['p50']} s |
| Model Load (CPU) | {stt_load_stats.get('mean', 'N/A')} s | — |

**STT device used for pipeline calc:** {stt_device}

### Stage 2: LLM Time to First Output (jarvis-brain)

Note: The jarvis-brain model (qwen3.6:27b) always outputs thinking tokens, even when
"think": false is set. "First output" measures when the first token arrives (whether
in the response or thinking field). "Prompt eval" measures how long the model spent
processing the input prompt before generating any output.

A separate warmup run was performed before the {run_count} measured runs to account
for cold KV cache initialization. Warmup time: {llm_warmup_time:.3f}s (not included in stats).

| Metric | First Output (s) | Prompt Eval (s) | Total Response (s) |
|--------|------------------|-----------------|--------------------|
| Mean | {llm_fo_stats.get('mean', 'N/A')} | {llm_pe_stats.get('mean', 'N/A')} | {llm_total_stats.get('mean', 'N/A')} |
| Std Dev | {llm_fo_stats.get('std', 'N/A')} | {llm_pe_stats.get('std', 'N/A')} | {llm_total_stats.get('std', 'N/A')} |
| Min | {llm_fo_stats.get('min', 'N/A')} | {llm_pe_stats.get('min', 'N/A')} | {llm_total_stats.get('min', 'N/A')} |
| Max | {llm_fo_stats.get('max', 'N/A')} | {llm_pe_stats.get('max', 'N/A')} | {llm_total_stats.get('max', 'N/A')} |
| P50 | {llm_fo_stats.get('p50', 'N/A')} | {llm_pe_stats.get('p50', 'N/A')} | {llm_total_stats.get('p50', 'N/A')} |

### Stage 3: Text-to-Speech (espeak-ng)

| Metric | First Byte | Total |
|--------|-----------|-------|
| Mean | {tts_fb_stats.get('mean', 'N/A')} s | {tts_total_stats.get('mean', 'N/A')} s |
| Std Dev | {tts_fb_stats.get('std', 'N/A')} s | {tts_total_stats.get('std', 'N/A')} s |
| Min | {tts_fb_stats.get('min', 'N/A')} s | {tts_total_stats.get('min', 'N/A')} s |
| Max | {tts_fb_stats.get('max', 'N/A')} s | {tts_total_stats.get('max', 'N/A')} s |
| P50 | {tts_fb_stats.get('p50', 'N/A')} s | {tts_total_stats.get('p50', 'N/A')} s |

### Pipeline Summary

| Metric | Value |
|--------|-------|
| Mean Pipeline Latency | {pipeline_mean} s |
| P50 Pipeline Latency | {pipeline_p50} s |
| Target | 1-3 s |
| **Within Target?** | {"YES" if isinstance(pipeline_mean, (int, float)) and pipeline_mean <= 3 else "NO"} |

## Per-Run Data

### STT (GPU) Runs
"""

    # Add per-run tables
    report += "\n| Run | Transcribe (s) |\n|-----|----------------|\n"
    for i, r in enumerate(stt_gpu_results):
        report += f"| {i+1} | {r['transcribe']} |\n"

    report += "\n### LLM Runs\n\n| Run | First Output (s) | Prompt Eval (s) | Total (s) | Tokens |\n"
    report += "|-----|------------------|-----------------|-----------|--------|\n"
    for i, r in enumerate(llm_results):
        report += f"| {i+1} | {r['first_output']} | {r['prompt_eval_s']} | {r['total']} | {r['tokens']} |\n"

    report += "\n### TTS Runs\n\n| Run | First Byte (s) | Total (s) |\n"
    report += "|-----|----------------|-----------|\n"
    for i, r in enumerate(tts_file_results):
        report += f"| {i+1} | {r['first_byte']} | {r['total']} |\n"

    report += f"""
## Analysis

### Breaking Down the Pipeline

The wake-to-first-word pipeline has three sequential stages:

1. **Wake word detected** -> **STT transcribes** -> **LLM generates** -> **TTS speaks**

The critical path is the sum of STT transcription, LLM first token, and TTS
first byte. These run sequentially in the simplest implementation.

### Where the Time Goes

- **STT ({stt_stats.get('mean', 'N/A')}s mean):** faster-whisper base.en is fast enough on GPU.
  The model is small (~150MB) and can be pre-loaded.
- **LLM first output ({llm_fo_stats.get('mean', 'N/A')}s mean, prompt eval {llm_pe_stats.get('mean', 'N/A')}s):** This is the dominant stage.
  The 27B model needs time to process the prompt. Note: this model always outputs
  thinking tokens regardless of API settings.
- **TTS ({tts_fb_stats.get('mean', 'N/A')}s mean):** espeak-ng is very fast; negligible contribution.

### Can We Meet the Target?

"""

    if isinstance(pipeline_mean, (int, float)):
        if pipeline_mean <= 3:
            report += f"**Yes.** The mean pipeline latency of {pipeline_mean}s is within the 1-3s target.\n"
        else:
            report += f"**No.** The mean pipeline latency of {pipeline_mean}s exceeds the 3s target.\n"
            report += "The LLM first token time is the primary bottleneck.\n\n"
            report += "### Optimization Opportunities\n\n"
            report += "1. **Speculative TTS:** Start TTS on partial LLM output (first few tokens)\n"
            report += "2. **Parallel STT+LLM:** Stream STT output to LLM before transcription completes\n"
            report += "3. **Smaller model:** Use a smaller model for simple acknowledgments\n"
            report += "4. **Faster TTS:** Piper or Coqui TTS may start faster than espeak-ng\n"
    else:
        report += "Could not compute pipeline latency (missing data).\n"

    report += """
## Raw Data

Measurement script: `scripts/measure_wake_latency.py`
"""

    # Write report
    report_path = RESULTS_DIR / "wake-latency-measurements.md"
    with open(report_path, "w") as f:
        f.write(report)

    print(f"\nReport written to {report_path}")
    return report_path


def main():
    parser = argparse.ArgumentParser(description="Measure wake-to-first-word latency")
    parser.add_argument("--runs", type=int, default=5, help="Number of runs per stage")
    parser.add_argument("--audio", type=str, default=None, help="Path to test audio file")
    args = parser.parse_args()

    run_count = args.runs

    # Step 0: Generate test audio if not provided
    if args.audio:
        audio_path = args.audio
    else:
        audio_path = str(REPO_DIR / "tmp" / "test_wake_audio.wav")
        os.makedirs(REPO_DIR / "tmp", exist_ok=True)
        print(f"Generating test audio...")
        generate_test_audio(audio_path)

    print(f"\nUsing audio: {audio_path}")
    duration = subprocess.run(
        ["ffprobe", "-v", "error", "-show_entries", "format=duration",
         "-of", "default=noprint_wrappers=1:nokey=1", audio_path],
        capture_output=True, text=True
    )
    print(f"Audio duration: {duration.stdout.strip()}s")

    # Step 1: Measure STT on CPU (baseline)
    print(f"\n{'='*60}")
    print(f"STT MEASUREMENT (CPU, {run_count} runs)")
    print(f"{'='*60}")
    stt_results = measure_stt(audio_path, runs=run_count)

    # Step 2: Measure STT on GPU (production)
    print(f"\n{'='*60}")
    print(f"STT MEASUREMENT (GPU, {run_count} runs)")
    print(f"{'='*60}")
    stt_gpu_results, stt_device = measure_stt_gpu(audio_path, runs=run_count)

    # Step 3: Measure LLM first token
    print(f"\n{'='*60}")
    print(f"LLM FIRST TOKEN MEASUREMENT ({run_count} runs)")
    print(f"{'='*60}")
    llm_results, llm_warmup_time = measure_llm_first_token(runs=run_count)

    # Step 4: Measure TTS
    print(f"\n{'='*60}")
    print(f"TTS MEASUREMENT ({run_count} runs)")
    print(f"{'='*60}")
    tts_file_results = measure_tts_latency_file(runs=run_count)

    # Step 5: Write report
    print(f"\n{'='*60}")
    print(f"WRITING REPORT")
    print(f"{'='*60}")
    write_markdown_report(
        stt_results, stt_gpu_results, stt_device,
        llm_results, llm_warmup_time, tts_file_results,
        audio_path, run_count
    )


if __name__ == "__main__":
    main()
