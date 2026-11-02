package planworker

import (
	"context"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// Engine is a local model engine (Ollama or a llama.cpp server) that a
// planner can run on. One engine per registry endpoint.
type Engine interface {
	// Loaded returns the names of the models currently loaded on the
	// engine, as the engine reports them.
	Loaded(ctx context.Context) ([]string, error)
	// Load makes model available on the engine.
	Load(ctx context.Context, model string) error
	// Unload evicts model from the engine.
	Unload(ctx context.Context, model string) error
	// Chat runs one completion against a loaded model and returns the
	// reply text.
	Chat(ctx context.Context, model string, messages []models.ChatMessage) (string, error)
}

// ClientEngine adapts a *models.Client to Engine.
type ClientEngine struct{ C *models.Client }

// planTimeout bounds one planning call: a 27B at ~48 tok/s with thinking
// on can write a plan for many minutes.
const planTimeout = 30 * time.Minute

// NewClientEngine builds an Engine for the engine at baseURL (a registry
// model's endpoint).
func NewClientEngine(baseURL string) *ClientEngine {
	return &ClientEngine{C: models.NewClientWithBaseTimeout(baseURL, planTimeout)}
}

func (e *ClientEngine) Loaded(ctx context.Context) ([]string, error) {
	infos, err := e.C.ModelPS()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(infos))
	for i, mi := range infos {
		names[i] = mi.Name
	}
	return names, nil
}

func (e *ClientEngine) Load(ctx context.Context, model string) error {
	return e.C.LoadModel(model, "")
}

func (e *ClientEngine) Unload(ctx context.Context, model string) error {
	return e.C.UnloadModel(model)
}

func (e *ClientEngine) Chat(ctx context.Context, model string, messages []models.ChatMessage) (string, error) {
	return e.C.Chat(model, messages)
}
