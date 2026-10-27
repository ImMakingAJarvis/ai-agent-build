package queue

import "strings"

// EffortOff disables model thinking (llama.cpp enable_thinking=false).
const EffortOff = "off"

// CanonicalEffort maps a job/Claude/SDK effort string onto values the local
// brain accepts. Empty or unknown values use defaultFor (voice → off, else medium).
//
// Claude Code CLI --effort accepts: low, medium, high, xhigh, max.
// The Qwen3.8 jinja template accepts: low, medium, xhigh (high maps to xhigh).
// "unknown" is what Claude Code sends for unrecognized model IDs and is not valid.
func CanonicalEffort(raw, defaultFor string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case EffortOff, "false", "none", "no", "0":
		return EffortOff
	case EffortLow:
		return EffortLow
	case EffortMedium:
		return EffortMedium
	case EffortHigh, EffortXHigh, EffortMax:
		return EffortXHigh
	case "", "unknown", "auto":
		return DefaultEffortFor(defaultFor)
	default:
		return DefaultEffortFor(defaultFor)
	}
}

// DefaultEffortFor is off for voice (and chat-like) jobs, medium for coding.
func DefaultEffortFor(jobType string) string {
	switch strings.ToLower(strings.TrimSpace(jobType)) {
	case VoiceQueue, "chat", "discord":
		return EffortOff
	default:
		return DefaultEffort
	}
}

// ClaudeEffort is the --effort / CLAUDE_CODE_EFFORT_LEVEL value.
// Off is empty (caller should omit the flag and disable thinking).
func ClaudeEffort(canonical string) string {
	switch canonical {
	case EffortOff:
		return ""
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return canonical
	default:
		return EffortMedium
	}
}

// EngineThinking is llama.cpp chat_template_kwargs.enable_thinking.
func EngineThinking(canonical string) bool {
	return canonical != EffortOff
}

// EngineReasoningEffort is llama.cpp chat_template_kwargs.reasoning_effort.
func EngineReasoningEffort(canonical string) string {
	c := CanonicalEffort(canonical, "")
	if c == EffortOff {
		return ""
	}
	if c == EffortHigh || c == EffortMax {
		return EffortXHigh
	}
	return c
}
