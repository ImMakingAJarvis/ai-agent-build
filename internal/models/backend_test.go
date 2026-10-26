package models_test

import (
	"os"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

func TestModelRegistryMoEModels(t *testing.T) {
	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	// Known MoE models should be identified.
	moeModels := []string{
		"qwen3.8-flash-next",
		"qwen3.8:flash-next",
		"Qwen/Qwen3.8-Flash-Next",
	}
	for _, name := range moeModels {
		if !reg.IsMoE(name) {
			t.Errorf("expected %q to be an MoE model", name)
		}
	}
}

func TestModelRegistryDenseModels(t *testing.T) {
	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	// Dense models should not be identified as MoE.
	denseModels := []string{
		"jarvis-brain",
		"qwen3.8:27b",
		"llama3.1:8b",
	}
	for _, name := range denseModels {
		if reg.IsMoE(name) {
			t.Errorf("expected %q to NOT be an MoE model", name)
		}
	}
}

func TestModelRegistryLookupMoE(t *testing.T) {
	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	cfg := reg.Lookup("qwen3.8-flash-next")
	if cfg.Backend != models.BackendFreeToken {
		t.Errorf("qwen3.8-flash-next should use freetoken backend, got %s", cfg.Backend)
	}
}

func TestModelRegistryLookupDense(t *testing.T) {
	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	cfg := reg.Lookup("jarvis-brain")
	if cfg.Backend != models.BackendLlamaCPP {
		t.Errorf("jarvis-brain should use llamacpp backend, got %s", cfg.Backend)
	}
}

func TestModelRegistryLookupUnknown(t *testing.T) {
	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	cfg := reg.Lookup("unknown-model")
	if cfg.Backend != models.BackendLlamaCPP {
		t.Errorf("unknown model should default to llamacpp backend, got %s", cfg.Backend)
	}
}

func TestModelRegistryCustomMoEModels(t *testing.T) {
	orig := os.Getenv("LOCAL_MOE_MODELS")
	os.Setenv("LOCAL_MOE_MODELS", "custom-moe-model,another-moe")
	defer os.Setenv("LOCAL_MOE_MODELS", orig)

	backends := models.DefaultBackendConfigs()
	reg := models.NewModelRegistry(backends)

	if !reg.IsMoE("custom-moe-model") {
		t.Error("custom-moe-model should be identified as MoE")
	}
	if !reg.IsMoE("another-moe") {
		t.Error("another-moe should be identified as MoE")
	}
}

func TestDefaultBackendConfigs(t *testing.T) {
	backends := models.DefaultBackendConfigs()

	if _, ok := backends[models.BackendLlamaCPP]; !ok {
		t.Error("default backends should include llamacpp")
	}
	if _, ok := backends[models.BackendFreeToken]; !ok {
		t.Error("default backends should include freetoken")
	}

	llama := backends[models.BackendLlamaCPP]
	if llama.URL == "" {
		t.Error("llamacpp backend URL should not be empty")
	}

	ft := backends[models.BackendFreeToken]
	if ft.URL == "" {
		t.Error("freetoken backend URL should not be empty")
	}
}

func TestBackendConfigEnvOverride(t *testing.T) {
	orig := os.Getenv("MOE_ENGINE_URL")
	os.Setenv("MOE_ENGINE_URL", "http://custom-host:9999")
	defer os.Setenv("MOE_ENGINE_URL", orig)

	backends := models.DefaultBackendConfigs()
	ft := backends[models.BackendFreeToken]
	if ft.URL != "http://custom-host:9999" {
		t.Errorf("MOE_ENGINE_URL should override freetoken URL, got %s", ft.URL)
	}
}
