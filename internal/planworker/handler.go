package planworker

import (
	"context"
	"errors"
	"fmt"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// Model is the live handle the planning pipeline talks to: the local
// engine with the planner loaded, or a cloud endpoint.
type Model interface {
	// ID is the registry id of the model.
	ID() string
	// Chat runs one completion against the model.
	Chat(ctx context.Context, messages []models.ChatMessage) (string, error)
	// Release returns the model to its pre-job state: for a swapped-in
	// local model it unloads the planner and reloads the previous model;
	// for a cloud model it is a no-op. Release runs even when the
	// pipeline fails, and its error is reported.
	Release(ctx context.Context) error
}

// Result is what a plan run produced.
type Result struct {
	// Plan is the pipeline's output (the plan document).
	Plan string
	// Model is the registry entry that ran the job.
	Model registry.Model
	// Local is true when the job ran on a local engine.
	Local bool
}

// Handler dispatches plan_epic jobs to the planner model named in them
// (docs/work-board/PLAN.md, "Model registry" loading rule): a local
// planner that is not loaded swaps it in — unload the current model, load
// the planner — and is restored after the run; a cloud planner runs on
// its API. The small always-on front-end model (FrontEnd) is never
// unloaded.
//
// The registry id is the name the worker calls the model by: a local
// engine must serve its models under their registry ids.
type Handler struct {
	// Models is the loaded model registry.
	Models *registry.File
	// Engines builds (or reuses) an engine client per registry endpoint.
	Engines func(endpoint string) Engine
	// Cloud calls cloud models; nil fails cloud jobs with a clear error.
	Cloud Cloud
	// Planner turns one job into a plan with the model it is handed.
	Planner Planner
	// FrontEnd is the small always-on front-end model that a swap must
	// never unload (e.g. the iGPU helper).
	FrontEnd string
}

// Run executes job: it dispatches to the model named in it, runs the
// pipeline, and releases the model (restoring the previous local model
// after a swap). Release errors are joined with the pipeline error.
func (h *Handler) Run(ctx context.Context, job *Job) (*Result, error) {
	m, err := h.model(job.ModelID)
	if err != nil {
		return nil, err
	}

	var model Model
	local := false
	if m.Location == "cloud" {
		model = &cloudModel{m: m, cloud: h.Cloud}
	} else {
		local = true
		model, err = h.localModel(ctx, m)
		if err != nil {
			return nil, err
		}
	}

	out, planErr := h.Planner.Plan(ctx, model, job)
	if relErr := model.Release(ctx); relErr != nil {
		if planErr != nil {
			planErr = errors.Join(planErr, relErr)
		} else {
			planErr = fmt.Errorf("release model %s: %w", m.ID, relErr)
		}
	}
	if planErr != nil {
		return nil, planErr
	}
	return &Result{Plan: out, Model: m, Local: local}, nil
}

// model looks up the job's planner in the registry. The planner only
// offers models tagged planner (PLAN.md), and disabled entries are not
// runnable.
func (h *Handler) model(id string) (registry.Model, error) {
	for i := range h.Models.Models {
		m := h.Models.Models[i]
		if m.ID != id {
			continue
		}
		if !m.Enabled {
			return m, fmt.Errorf("model %s is disabled in the registry", id)
		}
		for _, r := range m.Roles {
			if r == "planner" {
				return m, nil
			}
		}
		return m, fmt.Errorf("model %s is not tagged planner", id)
	}
	return registry.Model{}, fmt.Errorf("model %s not in registry", id)
}

// localModel returns a Model handle to m on its engine, swapping the model
// in when it is not loaded: the current work model is unloaded (never the
// front-end model), the planner is loaded, and the handle's Release
// restores the previous model.
func (h *Handler) localModel(ctx context.Context, m registry.Model) (Model, error) {
	eng := h.Engines(m.Endpoint)
	loaded, err := eng.Loaded(ctx)
	if err != nil {
		return nil, fmt.Errorf("list loaded models on %s: %w", m.Endpoint, err)
	}
	if contains(loaded, m.ID) {
		return &localModel{eng: eng, id: m.ID}, nil
	}

	// Swap: make room by unloading the current work model.
	prev := workModel(loaded, h.FrontEnd)
	if prev != "" {
		if err := eng.Unload(ctx, prev); err != nil {
			return nil, fmt.Errorf("unload %s before planner %s: %w", prev, m.ID, err)
		}
	}
	if err := eng.Load(ctx, m.ID); err != nil {
		// Best-effort restore: the engine must not be left with the
		// previous model unloaded and no planner loaded.
		if prev != "" {
			if rerr := eng.Load(ctx, prev); rerr != nil {
				return nil, errors.Join(fmt.Errorf("load planner %s: %w", m.ID, err),
					fmt.Errorf("restore %s after failed load: %w", prev, rerr))
			}
		}
		return nil, fmt.Errorf("load planner %s: %w", m.ID, err)
	}
	return &localModel{eng: eng, id: m.ID, prev: prev, swapped: true}, nil
}

// workModel is the model a swap must make room for: the first loaded model
// that is not the always-on front-end model.
func workModel(loaded []string, frontEnd string) string {
	for _, n := range loaded {
		if n != frontEnd {
			return n
		}
	}
	return ""
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// localModel is a planner loaded on its engine, possibly swapped in over
// a previous model.
type localModel struct {
	eng     Engine
	id      string
	prev    string // the model Release restores ("" when none)
	swapped bool
}

func (l *localModel) ID() string { return l.id }

func (l *localModel) Chat(ctx context.Context, messages []models.ChatMessage) (string, error) {
	return l.eng.Chat(ctx, l.id, messages)
}

func (l *localModel) Release(ctx context.Context) error {
	if !l.swapped {
		return nil
	}
	var errs []error
	if err := l.eng.Unload(ctx, l.id); err != nil {
		errs = append(errs, fmt.Errorf("unload planner %s: %w", l.id, err))
	}
	if l.prev != "" {
		if err := l.eng.Load(ctx, l.prev); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", l.prev, err))
		}
	}
	return errors.Join(errs...)
}

// cloudModel is a planner on its cloud API; it holds no resources.
type cloudModel struct {
	m     registry.Model
	cloud Cloud
}

func (c *cloudModel) ID() string { return c.m.ID }

func (c *cloudModel) Chat(ctx context.Context, messages []models.ChatMessage) (string, error) {
	return c.cloud.Chat(ctx, &c.m, messages)
}

func (c *cloudModel) Release(ctx context.Context) error { return nil }
