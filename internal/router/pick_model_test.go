package router_test

import (
	"os"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/router"
)

func TestPickModelVoiceLocal(t *testing.T) {
	name, useLocal := router.PickModel("voice", false, "", true)
	if !useLocal {
		t.Error("voice job should use local model")
	}
	if name == "" {
		t.Error("model name should not be empty")
	}
}

func TestPickModelForceAPI(t *testing.T) {
	name, useLocal := router.PickModel("voice", true, "", true)
	if useLocal {
		t.Error("forceAPI should not use local model")
	}
	if name == "" {
		t.Error("model name should not be empty")
	}
}

func TestPickModelOverride(t *testing.T) {
	name, useLocal := router.PickModel("coding", false, "claude-opus-5-5", true)
	if name != "claude-opus-5-5" {
		t.Errorf("model override should be claude-opus-5-5, got %s", name)
	}
	if !useLocal {
		t.Error("model override should set useLocal=true (it's the model name we want)")
	}
}

func TestPickModelLocalUnavailable(t *testing.T) {
	name, useLocal := router.PickModel("voice", false, "", false)
	if useLocal {
		t.Error("should fall back to API when local is unavailable")
	}
	if name == "" {
		t.Error("fallback model name should not be empty")
	}
}

func TestPickModelCoding(t *testing.T) {
	name, useLocal := router.PickModel("coding", false, "", true)
	if !useLocal {
		t.Error("coding job should use local model when available")
	}
	if name == "" {
		t.Error("model name should not be empty")
	}
}

func TestPickModelBackground(t *testing.T) {
	_, useLocal := router.PickModel("background", false, "", true)
	if !useLocal {
		t.Error("background job should use local model when available")
	}
}

func TestPickModelWithPayloadForceAPI(t *testing.T) {
	_, useLocal := router.PickModelWithPayload("voice", true, "", true)
	if useLocal {
		t.Error("forceAPI should prevent local model use")
	}
}

func TestPickModelWithPayloadOverride(t *testing.T) {
	name, _ := router.PickModelWithPayload("coding", false, "claude-opus-5-5", true)
	if name != "claude-opus-5-5" {
		t.Errorf("modelOverride should set model to claude-opus-5-5, got %s", name)
	}
}

func TestPickModelEnvOverride(t *testing.T) {
	orig := os.Getenv("LOCAL_MODEL")
	os.Setenv("LOCAL_MODEL", "test-model")
	defer os.Setenv("LOCAL_MODEL", orig)

	name, useLocal := router.PickModel("voice", false, "", true)
	if !useLocal {
		t.Error("should use local")
	}
	if name != "test-model" {
		t.Errorf("should respect LOCAL_MODEL env, got %s", name)
	}
}

func TestPickModelAPIModelEnv(t *testing.T) {
	orig := os.Getenv("API_MODEL")
	os.Setenv("API_MODEL", "claude-opus-5-5")
	defer os.Setenv("API_MODEL", orig)

	name, useLocal := router.PickModel("voice", true, "", true)
	if useLocal {
		t.Error("forceAPI should not use local")
	}
	if name != "claude-opus-5-5" {
		t.Errorf("should respect API_MODEL env, got %s", name)
	}
}

func TestPickModelVoiceUsesLightModel(t *testing.T) {
	origLight := os.Getenv("LOCAL_LIGHT_MODEL")
	origModel := os.Getenv("LOCAL_MODEL")
	os.Setenv("LOCAL_LIGHT_MODEL", "qwen2.5:7b")
	os.Setenv("LOCAL_MODEL", "jarvis-brain")
	defer func() {
		os.Setenv("LOCAL_LIGHT_MODEL", origLight)
		os.Setenv("LOCAL_MODEL", origModel)
	}()

	name, useLocal := router.PickModel("voice", false, "", true)
	if !useLocal {
		t.Error("voice job should use local")
	}
	if name != "qwen2.5:7b" {
		t.Errorf("voice should use LOCAL_LIGHT_MODEL, got %s", name)
	}
}

func TestPickModelVoiceLightModelDefaultsToLocal(t *testing.T) {
	origLight := os.Getenv("LOCAL_LIGHT_MODEL")
	origModel := os.Getenv("LOCAL_MODEL")
	os.Unsetenv("LOCAL_LIGHT_MODEL")
	os.Setenv("LOCAL_MODEL", "jarvis-brain")
	defer func() {
		os.Setenv("LOCAL_LIGHT_MODEL", origLight)
		os.Setenv("LOCAL_MODEL", origModel)
	}()

	name, useLocal := router.PickModel("voice", false, "", true)
	if !useLocal {
		t.Error("voice job should use local")
	}
	if name != "jarvis-brain" {
		t.Errorf("voice should fall back to LOCAL_MODEL when LOCAL_LIGHT_MODEL is unset, got %s", name)
	}
}

func TestPickModelCodingIgnoresLightModel(t *testing.T) {
	origLight := os.Getenv("LOCAL_LIGHT_MODEL")
	origModel := os.Getenv("LOCAL_MODEL")
	os.Setenv("LOCAL_LIGHT_MODEL", "qwen2.5:7b")
	os.Setenv("LOCAL_MODEL", "jarvis-brain")
	defer func() {
		os.Setenv("LOCAL_LIGHT_MODEL", origLight)
		os.Setenv("LOCAL_MODEL", origModel)
	}()

	name, useLocal := router.PickModel("coding", false, "", true)
	if !useLocal {
		t.Error("coding job should use local")
	}
	if name != "jarvis-brain" {
		t.Errorf("coding should use LOCAL_MODEL, not LOCAL_LIGHT_MODEL, got %s", name)
	}
}

func TestPickModelBackgroundIgnoresLightModel(t *testing.T) {
	origLight := os.Getenv("LOCAL_LIGHT_MODEL")
	origModel := os.Getenv("LOCAL_MODEL")
	os.Setenv("LOCAL_LIGHT_MODEL", "qwen2.5:7b")
	os.Setenv("LOCAL_MODEL", "jarvis-brain")
	defer func() {
		os.Setenv("LOCAL_LIGHT_MODEL", origLight)
		os.Setenv("LOCAL_MODEL", origModel)
	}()

	name, useLocal := router.PickModel("background", false, "", true)
	if !useLocal {
		t.Error("background job should use local")
	}
	if name != "jarvis-brain" {
		t.Errorf("background should use LOCAL_MODEL, got %s", name)
	}
}
