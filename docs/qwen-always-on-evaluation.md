# Qwen as Primary Always-On AI: Evaluation

**Date:** 2026-10-21
**Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama 0.40.7
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM), 128GB RAM, Ubuntu
**Next Version:** qwen3.8:27b (queued as job Q1)

## Question

Is the best Qwen model close enough to top-tier reasoning to serve as the primary always-on AI managing Home Assistant, with a heavier coding model swapped in only for queued jobs?

## Short Answer

**Yes, with one caveat.** Qwen 3.6:27b has sufficient reasoning quality for Home Assistant management as the primary always-on agent. The single material gap is silent failures on real-time data queries (time, weather, sensor reads), which is a tool-access problem, not a reasoning deficit.

## Evidence

### 1. Reasoning Quality (Direct HA Automation Test)

The model was given a Home Assistant automation debugging task: a YAML config where a light triggers every day at 6 PM regardless of season. The model:

- Correctly identified the bug: a `time` trigger fires at 18:00 every day without considering sunset
- Explained why it's problematic: summer vs. winter differences
- Suggested the HA-native solution: a `sun` trigger with `offset` parameter
- Produced properly formatted YAML with correct Home Assistant syntax
- Structured the response with clear sections (problem, explanation, solution)

**Verdict:** Strong reasoning. The model understands HA domain concepts, debugs misconfigurations, and produces correct YAML suggestions.

### 2. Voice/Conversational Ability (Sonnet-Graded, 25 Tasks)

| Category | Score | Notes |
|----------|-------|-------|
| Home Assistant commands | 4.4/5 | Excellent at turn-on/off/dim/lock commands |
| Follow-up questions | 4.8/5 | Near-perfect context retention |
| Multi-turn conversation | 3.8/5 | Good but verbose on complex tasks |
| Short answers | 3.3/5 | Dragged down by silent failures |
| **Overall** | **4.1/5** | |

**Verdict:** Strong conversational ability for voice interaction. HA command handling is production-quality.

### 3. Critical Issue: Silent Failures on Real-Time Queries

Five complete non-responses (17% failure rate), all on queries requiring real-time data:

| Task | Query Type | Result |
|------|-----------|--------|
| ha-03 | "What's the temperature in the living room right now?" | No response |
| sa-01 | "What time is it?" | No response |
| sa-05 | "Is it going to rain tomorrow?" | No response |
| sa-06 | "What day of the week is October 21st, 2026?" | No response |
| mt-04 | "I've been feeling really tired lately..." | No response |

**Root cause:** This is not a reasoning failure. The model has no access to real-time data (clock, weather API, Home Assistant sensor states) without tool integrations. When asked for information it cannot compute from training data alone, it produces empty responses instead of hedging or explaining its limitations.

**Fix path:** This requires tool-use integration (Phase 4: Home Assistant API access, time system prompt injection), not a model change. A better model won't magically know the current time or sensor values.

### 4. Latency

| Stage | Mean | Target |
|-------|------|--------|
| STT (faster-whisper GPU) | 0.087s | < 0.5s |
| LLM first token | 0.113s | < 1s |
| TTS (espeak-ng) | 0.006s | < 0.5s |
| **Pipeline** | **0.206s** | **1-3s** |

**Verdict:** Well within voice interaction targets. Thinking mode is ON (adds ~0.18s median, cannot be suppressed on this model).

### 5. Hardware Viability

| Metric | Value |
|--------|-------|
| Model in VRAM | 22.7 GB / 24.5 GB |
| Idle power overhead | +14W ($14/year) |
| Cold start time | 8.6s (too slow for voice) |
| Model stays resident | Yes (required) |

**Verdict:** Hardware is sufficient. No separate tiny always-on model needed.

### 6. Qwen 3.8 Availability

Qwen 3.8 (`qwen3.8:27b`) is the next version, already queued as job Q1. Same 27B parameter count, same VRAM footprint. If 3.6 passes this evaluation, 3.8 should improve on reasoning without changing the hardware equation.

## Conclusion

| Criterion | Result | Pass? |
|-----------|--------|-------|
| Reasoning quality for HA management | Strong YAML debugging, domain understanding | Yes |
| Voice/conversational ability | 4.1/5 overall | Yes |
| Latency for voice interaction | 0.206s pipeline | Yes |
| Hardware viability | Fits in VRAM, $14/year idle cost | Yes |
| Real-time data queries | Silent failures without tool access | Caveat |

**Recommendation:** Qwen is suitable as the primary always-on AI for Home Assistant management. The silent failure on real-time queries is an integration gap (needs tool access to HA APIs and system time), not a model quality issue. Proceed with Phase 4 Home Assistant integration to address the real-time data gap.

The heavier coding model should remain swapped in only for queued coding jobs, as originally planned.

## Related Documents

- [[voice-test-results]]: Full Sonnet-graded voice test results
- [[thinking-comparison]]: Thinking ON vs OFF latency comparison
- [[wake-latency-measurements]]: Stage-by-stage pipeline latency
- [[model-load-time-measurements]]: Cold start measurements
- [[gpu-power-measurements]]: Idle power and cost analysis
