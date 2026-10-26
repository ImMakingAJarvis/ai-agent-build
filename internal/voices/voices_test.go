package voices

import "testing"

func TestGetDefaultVoice(t *testing.T) {
	v := Get("")
	if v == nil {
		t.Fatal("default voice is nil")
	}
	if v.Key != "jarvis" {
		t.Errorf("default voice key = %q; want %q", v.Key, "jarvis")
	}
}

func TestGetGLaDOS(t *testing.T) {
	v := Get("glados")
	if v == nil {
		t.Fatal("glados voice is nil")
	}
	if v.Key != "glados" {
		t.Errorf("glados key = %q; want %q", v.Key, "glados")
	}
	if v.Name != "GLaDOS" {
		t.Errorf("glados name = %q; want %q", v.Name, "GLaDOS")
	}
	if v.SystemPrompt == "" {
		t.Error("glados system prompt is empty")
	}
}

func TestGetUnknownReturnsDefault(t *testing.T) {
	v := Get("nonexistent")
	if v == nil {
		t.Fatal("unknown voice returned nil")
	}
	if v.Key != "jarvis" {
		t.Errorf("unknown voice fell back to %q; want %q", v.Key, "jarvis")
	}
}

func TestKeys(t *testing.T) {
	keys := Keys()
	if len(keys) < 2 {
		t.Errorf("expected at least 2 voices; got %d", len(keys))
	}
	hasJarvis := false
	hasGLaDOS := false
	for _, k := range keys {
		if k == "jarvis" {
			hasJarvis = true
		}
		if k == "glados" {
			hasGLaDOS = true
		}
	}
	if !hasJarvis {
		t.Error("missing jarvis voice in keys")
	}
	if !hasGLaDOS {
		t.Error("missing glados voice in keys")
	}
}

func TestRegisterNewVoice(t *testing.T) {
	Register(&Voice{
		Key:          "test-voice",
		Name:         "Test",
		SystemPrompt: "test prompt",
	})
	v := Get("test-voice")
	if v == nil {
		t.Fatal("registered voice not found")
	}
	if v.SystemPrompt != "test prompt" {
		t.Errorf("prompt = %q; want %q", v.SystemPrompt, "test prompt")
	}
}
