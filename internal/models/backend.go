// Package models provides clients for local model inference backends.
//
// It supports two backends:
//   - llama.cpp (via Ollama-compatible API) for dense models
//   - FreeToken for Mixture-of-Experts (MoE) models
//
// The ModelRegistry routes model names to the appropriate backend.
package models

import (
	"os"
)

// Backend identifies which inference engine handles a model.
type Backend string

const (
	// BackendLlamaCPP routes to the llama.cpp server (Ollama-compatible API).
	BackendLlamaCPP Backend = "llamacpp"
	// BackendFreeToken routes to the FreeToken server (OpenAI-compatible API).
	BackendFreeToken Backend = "freetoken"
)

// BackendConfig holds the connection details for an inference backend.
type BackendConfig struct {
	Backend Backend
	URL     string
}

// DefaultBackendConfigs returns the backend configurations from environment
// variables, falling back to defaults.
//
// ENGINE_URL — llama.cpp base URL (default: http://localhost:11434)
// MOE_ENGINE_URL — FreeToken base URL (default: http://localhost:11435)
func DefaultBackendConfigs() map[Backend]BackendConfig {
	return map[Backend]BackendConfig{
		BackendLlamaCPP: {
			Backend: BackendLlamaCPP,
			URL:     getEnv("ENGINE_URL", "http://localhost:11434"),
		},
		BackendFreeToken: {
			Backend: BackendFreeToken,
			URL:     getEnv("MOE_ENGINE_URL", "http://localhost:11435"),
		},
	}
}

// ModelRegistry maps model names to their inference backend.
type ModelRegistry struct {
	// modelBackend maps a model name to its backend configuration.
	modelBackend map[string]BackendConfig
	// moeModels is a set of model names that should use FreeToken.
	moeModels map[string]struct{}
	// defaultBackend is the fallback backend for unregistered models.
	defaultBackend BackendConfig
}

// NewModelRegistry creates a registry with the given backend configs.
// MoE models listed in the LOCAL_MOE_MODELS env var (comma-separated) are
// routed to FreeToken; all others go to llama.cpp.
func NewModelRegistry(backends map[Backend]BackendConfig) *ModelRegistry {
	mr := &ModelRegistry{
		modelBackend:   make(map[string]BackendConfig),
		moeModels:      make(map[string]struct{}),
		defaultBackend: backends[BackendLlamaCPP],
	}

	// Register well-known MoE models.
	mr.registerMoEModels()

	// Parse LOCAL_MOE_MODELS env var for custom MoE model names.
	if moeList := os.Getenv("LOCAL_MOE_MODELS"); moeList != "" {
		for _, name := range splitComma(moeList) {
			mr.moeModels[name] = struct{}{}
		}
	}

	// Register configured local models.
	mr.registerLocalModels()

	return mr
}

// registerMoEModels adds known MoE models to the registry.
func (mr *ModelRegistry) registerMoEModels() {
	// Qwen 3.8 Flash Next is a 125B MoE model.
	mr.moeModels["qwen3.8-flash-next"] = struct{}{}
	mr.moeModels["qwen3.8:flash-next"] = struct{}{}
	mr.moeModels["Qwen/Qwen3.8-Flash-Next"] = struct{}{}
}

// registerLocalModels registers the configured local models with their
// appropriate backends based on whether they are MoE models.
func (mr *ModelRegistry) registerLocalModels() {
	backends := DefaultBackendConfigs()
	llamaURL := backends[BackendLlamaCPP].URL
	freetokenURL := backends[BackendFreeToken].URL

	models := []struct {
		name string
		env  string
	}{
		{os.Getenv("LOCAL_MODEL"), "LOCAL_MODEL"},
		{os.Getenv("LOCAL_LIGHT_MODEL"), "LOCAL_LIGHT_MODEL"},
		{os.Getenv("LOCAL_CODER_MODEL"), "LOCAL_CODER_MODEL"},
	}

	for _, m := range models {
		if m.name == "" {
			continue
		}
		if _, isMoE := mr.moeModels[m.name]; isMoE {
			mr.modelBackend[m.name] = BackendConfig{
				Backend: BackendFreeToken,
				URL:     freetokenURL,
			}
		} else {
			mr.modelBackend[m.name] = BackendConfig{
				Backend: BackendLlamaCPP,
				URL:     llamaURL,
			}
		}
	}
}

// Lookup returns the backend configuration for a model name.
// If the model is not registered, returns the default backend (llama.cpp).
// Known MoE models are always routed to FreeToken regardless of registration.
func (mr *ModelRegistry) Lookup(modelName string) BackendConfig {
	if cfg, ok := mr.modelBackend[modelName]; ok {
		return cfg
	}
	// Check if it's a known MoE model — route to FreeToken.
	if _, isMoE := mr.moeModels[modelName]; isMoE {
		backends := DefaultBackendConfigs()
		return backends[BackendFreeToken]
	}
	return mr.defaultBackend
}

// IsMoE returns whether a model is a Mixture-of-Experts model.
func (mr *ModelRegistry) IsMoE(modelName string) bool {
	_, ok := mr.moeModels[modelName]
	return ok
}

// ---------- helpers ----------

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitComma(s string) []string {
	var result []string
	for _, part := range split(s, ',') {
		p := trim(part)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func split(s string, sep rune) []string {
	var result []string
	var current []rune
	for _, r := range s {
		if r == sep {
			result = append(result, string(current))
			current = nil
		} else {
			current = append(current, r)
		}
	}
	result = append(result, string(current))
	return result
}

func trim(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
