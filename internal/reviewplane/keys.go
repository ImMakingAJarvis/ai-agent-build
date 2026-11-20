// Package reviewplane gives each model its own Plane identity (Job 11 of
// docs/pr-review-build-plan.md): one Plane API key per model in the queue
// site secret manager, and the rule that decides which model a Plane
// comment is for. Comments are posted with the model's own key — never the
// shared PLANE_API_KEY — so Plane's created_by says which model spoke.
package reviewplane

import (
	"context"
	"fmt"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/secrets"
)

// keyNames maps a model id (or its driver id) to the secret that holds its
// Plane API key. The names are fixed: the secret manager stores exactly
// these five (Job 11a). Haiku has a key and no review queue in this plan.
var keyNames = map[string]string{
	"sonnet":            "plane-sonnet-bot",
	"claude-sonnet-5-5": "plane-sonnet-bot",
	"opus":              "plane-opus-bot",
	"fable":             "plane-fable-bot",
	"claude-fable-5-1":  "plane-fable-bot",
	"haiku":             "plane-haiku-bot",
	"qwen3.8-27b":       "plane-jarvis-big-brain",
}

// KeyName returns the secret name that holds the Plane API key for modelID,
// or "" for a model that has no key in this plan.
func KeyName(modelID string) string { return keyNames[modelID] }

// HasKey reports whether modelID's Plane key is stored. It uses the store's
// List — names only, never values; a missing key returns false.
func HasKey(store *secrets.Store, modelID string) (bool, error) {
	name := KeyName(modelID)
	if name == "" {
		return false, nil
	}
	infos, err := store.List()
	if err != nil {
		return false, err
	}
	for _, info := range infos {
		if info.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// PostComment posts html on the work item as the Plane user of modelID,
// signed with that model's own secret key. A missing key is an error that
// names the secret; there is no fallback to PLANE_API_KEY. baseURL and
// workspace are taken as plain values on purpose — the caller's env (which
// may hold PLANE_API_KEY) never reaches the client.
func PostComment(ctx context.Context, baseURL, workspace string, store *secrets.Store, modelID, projectID, itemID, html string) (*plane.Comment, error) {
	name := KeyName(modelID)
	if name == "" {
		return nil, fmt.Errorf("reviewplane: model %q has no Plane key in this plan", modelID)
	}
	ok, err := HasKey(store, modelID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("reviewplane: no Plane key stored under %q for model %q", name, modelID)
	}
	key, err := store.Get(name)
	if err != nil {
		return nil, fmt.Errorf("reviewplane: read %q: %w", name, err)
	}
	client, err := plane.New(plane.Config{BaseURL: baseURL, Workspace: workspace, APIKey: key})
	if err != nil {
		return nil, err
	}
	return client.AddComment(ctx, projectID, itemID, html)
}
