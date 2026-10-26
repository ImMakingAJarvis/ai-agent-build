package voices

import (
	"strings"
	"testing"
)

func TestGLaDOSVoiceContent(t *testing.T) {
	v := Get("glados")
	if v == nil {
		t.Fatal("glados voice not found")
	}

	prompt := v.SystemPrompt

	// Should identify as GLaDOS.
	if !strings.Contains(prompt, "GLaDOS") {
		t.Error("GLaDOS prompt should mention GLaDOS")
	}

	// Should still include the safety constraints.
	if !strings.Contains(strings.ToLower(prompt), "no shell") {
		t.Error("GLaDOS prompt should ban shell access")
	}
	if !strings.Contains(strings.ToLower(prompt), "no secrets") {
		t.Error("GLaDOS prompt should ban secrets access")
	}

	// Should include personality guidelines.
	if !strings.Contains(prompt, "Sarcastic") {
		t.Error("GLaDOS prompt should mention sarcastic tone")
	}
	if !strings.Contains(prompt, "SCHEDULE") {
		t.Error("GLaDOS prompt should include SCHEDULE format")
	}
}

func TestJarvisVoiceContent(t *testing.T) {
	v := Get("jarvis")
	if v == nil {
		t.Fatal("jarvis voice not found")
	}

	prompt := v.SystemPrompt

	// Should identify as Jarvis.
	if !strings.Contains(prompt, "Jarvis") {
		t.Error("Jarvis prompt should mention Jarvis")
	}

	// Should include safety constraints.
	if !strings.Contains(strings.ToLower(prompt), "no shell") {
		t.Error("Jarvis prompt should ban shell access")
	}
}
