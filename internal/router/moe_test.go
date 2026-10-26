package router_test

import (
	"os"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/router"
)

func TestIsMoEModel(t *testing.T) {
	moeModels := []string{
		"qwen3.8-flash-next",
		"qwen3.8:flash-next",
		"Qwen/Qwen3.8-Flash-Next",
	}
	for _, name := range moeModels {
		if !router.IsMoEModel(name) {
			t.Errorf("expected %q to be identified as MoE model", name)
		}
	}
}

func TestIsMoEModelDense(t *testing.T) {
	denseModels := []string{
		"jarvis-brain",
		"qwen3.8:27b",
		"llama3.1:8b",
		"gpt-4",
	}
	for _, name := range denseModels {
		if router.IsMoEModel(name) {
			t.Errorf("expected %q to NOT be identified as MoE model", name)
		}
	}
}

func TestIsMoEModelCustomEnv(t *testing.T) {
	orig := os.Getenv("LOCAL_MOE_MODELS")
	os.Setenv("LOCAL_MOE_MODELS", "my-custom-moe")
	defer os.Setenv("LOCAL_MOE_MODELS", orig)

	if !router.IsMoEModel("my-custom-moe") {
		t.Error("my-custom-moe should be identified as MoE from LOCAL_MOE_MODELS env")
	}
	// Known dense models should still not be MoE.
	if router.IsMoEModel("jarvis-brain") {
		t.Error("jarvis-brain should not be MoE even with custom LOCAL_MOE_MODELS")
	}
}
