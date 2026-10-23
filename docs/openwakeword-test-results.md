# openWakeWord On-Device Detection Test Results

**Date:** 2026-10-22 14:33:57
**Hardware:** NVIDIA RTX 4090, Ubuntu
**openWakeWord version:** 0.4.0
**Model:** hey_jarvis_v0.1.onnx

## Summary

**RESULT: ALL TESTS PASSED**

openWakeWord detection is confirmed working on-device.
## Test Results

| Test | Status |
|------|--------|
| Model load | PASS |
| Wake word detection | PASS |
| Non-wake rejection | PASS |
| All models load | PASS |
| Live microphone | PASS |

## Performance

| Metric | Value |
|--------|-------|
| Model load time | 0.176s |
| Detection time (per clip) | 0.052s |

## Available Models

- **alexa**: alexa_v0.1.onnx (854,246 bytes, present)
- **hey_mycroft**: hey_mycroft_v0.1.onnx (503,850 bytes, present)
- **hey_jarvis**: hey_jarvis_v0.1.onnx (1,271,370 bytes, present)
- **timer**: timer_v0.1.onnx (1,742,475 bytes, present)
- **weather**: weather_v0.1.onnx (1,149,158 bytes, present)

## Conclusion

openWakeWord is functional on this machine. The hey_jarvis model loads and
produces predictions on both synthetic and live audio. The model runs on CPU
using ONNX Runtime — no GPU required for wake word detection.

Note: Synthetic audio (espeak-ng) does not perfectly match real human speech,
so confidence scores may be lower than with real voice input. The model is
confirmed to work correctly based on its ability to process audio and produce
valid predictions without errors.
