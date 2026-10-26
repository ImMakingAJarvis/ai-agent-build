package models

import (
	"fmt"
	"log"
	"os"
	"sync"
)

// UnifiedClient routes chat requests to the appropriate backend
// (llama.cpp or FreeToken) based on the model name.
type UnifiedClient struct {
	mu       sync.Mutex
	registry *ModelRegistry
	llama    *Client
	freetoken *FreeTokenClient
}

// NewUnifiedClient creates a client that routes to llama.cpp or FreeToken
// based on the model name.
func NewUnifiedClient() *UnifiedClient {
	backends := DefaultBackendConfigs()
	return &UnifiedClient{
		registry:  NewModelRegistry(backends),
		llama:     NewClient(),
		freetoken: NewFreeTokenClient(),
	}
}

// Chat routes a chat request to the appropriate backend for the given model.
func (uc *UnifiedClient) Chat(model string, messages []ChatMessage) (string, error) {
	cfg := uc.registry.Lookup(model)

	switch cfg.Backend {
	case BackendFreeToken:
		return uc.chatFreeToken(model, messages)
	case BackendLlamaCPP:
		return uc.chatLlama(model, messages)
	default:
		return "", fmt.Errorf("unknown backend %q for model %q", cfg.Backend, model)
	}
}

// chatFreeToken converts ChatMessages to FreeTokenChatMessages and calls
// the FreeToken server.
func (uc *UnifiedClient) chatFreeToken(model string, messages []ChatMessage) (string, error) {
	ftMessages := make([]FreeTokenChatMessage, len(messages))
	for i, m := range messages {
		ftMessages[i] = FreeTokenChatMessage{
			Role:    m.Role,
			Content: m.Content,
		}
	}
	return uc.freetoken.FreeTokenChat(model, ftMessages)
}

// chatLlama routes to the llama.cpp/Ollama backend.
func (uc *UnifiedClient) chatLlama(model string, messages []ChatMessage) (string, error) {
	return uc.llama.Chat(model, messages)
}

// LookupBackend returns the backend configuration for a model.
func (uc *UnifiedClient) LookupBackend(model string) BackendConfig {
	return uc.registry.Lookup(model)
}

// IsMoE returns whether a model uses the FreeToken backend.
func (uc *UnifiedClient) IsMoE(model string) bool {
	return uc.registry.IsMoE(model)
}

// HealthCheckLlama checks if the llama.cpp backend is healthy.
func (uc *UnifiedClient) HealthCheckLlama() bool {
	return uc.llama.Healthcheck()
}

// HealthCheckFreeToken checks if the FreeToken backend is healthy.
func (uc *UnifiedClient) HealthCheckFreeToken() bool {
	return uc.freetoken.FreeTokenHealthCheck()
}

// HealthCheck checks the appropriate backend for the given model.
func (uc *UnifiedClient) HealthCheck(model string) bool {
	cfg := uc.registry.Lookup(model)
	switch cfg.Backend {
	case BackendFreeToken:
		return uc.HealthCheckFreeToken()
	case BackendLlamaCPP:
		return uc.HealthCheckLlama()
	default:
		return false
	}
}

// GetRegistry returns the model registry for inspection.
func (uc *UnifiedClient) GetRegistry() *ModelRegistry {
	return uc.registry
}

// GetLlamaClient returns the underlying llama.cpp client.
func (uc *UnifiedClient) GetLlamaClient() *Client {
	return uc.llama
}

// GetFreeTokenClient returns the underlying FreeToken client.
func (uc *UnifiedClient) GetFreeTokenClient() *FreeTokenClient {
	return uc.freetoken
}

// BackendStatus holds the status of each backend.
type BackendStatus struct {
	LlamaCPPLive   bool
	FreeTokenLive  bool
	LlamaCPPOk     bool
	FreeTokenOk    bool
	DefaultBackend Backend
}

// Status returns the health status of all configured backends.
func (uc *UnifiedClient) Status() BackendStatus {
	return BackendStatus{
		LlamaCPPLive:  uc.HealthCheckLlama(),
		FreeTokenLive: uc.HealthCheckFreeToken(),
		LlamaCPPOk:    uc.HealthCheckLlama(),
		FreeTokenOk:   uc.HealthCheckFreeToken(),
		DefaultBackend: BackendLlamaCPP,
	}
}

// LogBackendInfo logs the backend configuration at startup.
func LogBackendInfo() {
	backends := DefaultBackendConfigs()
	log.Printf("models: llama.cpp backend at %s", backends[BackendLlamaCPP].URL)
	log.Printf("models: freetoken backend at %s", backends[BackendFreeToken].URL)

	if moeModels := os.Getenv("LOCAL_MOE_MODELS"); moeModels != "" {
		log.Printf("models: custom MoE models: %s", moeModels)
	}
}
