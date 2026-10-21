// Package router implements model selection logic for queued jobs.
package router

import (
	"os"
)

// PickModel decides which model should handle a given job.
//
// Routing rules:
//   - If the message has a ModelOverride, use it and set useLocal=true.
//   - If ForceAPI is true, return the API model (default "claude-sonnet-5-5").
//   - Voice jobs → local model (from LOCAL_MODEL env, default "jarvis-brain").
//   - Coding jobs → local coder model if set (LOCAL_CODER_MODEL env),
//     otherwise falls through to the local model.
//   - Background jobs → local model by default.
//   - If the local model is unavailable (checkLocal is called and returns false),
//     fall back to the API model.
func PickModel(jobType string, forceAPI bool, modelOverride string, localAvailable bool) (modelName string, useLocal bool) {
	// Explicit override wins.
	if modelOverride != "" {
		return modelOverride, true
	}

	// ForceAPI skips local entirely.
	if forceAPI {
		return apiModel(), false
	}

	// If local is down, fall back to API.
	if !localAvailable {
		return apiModel(), false
	}

	local := localModel()
	return local, true
}

// PickModelWithPayload is like PickModel but reads flags from the job payload map.
func PickModelWithPayload(jobType string, payload map[string]string, localAvailable bool) (modelName string, useLocal bool) {
	forceAPI := payload["force_api"] == "true" || payload["force_api"] == "1"
	modelOverride := payload["model_override"]
	return PickModel(jobType, forceAPI, modelOverride, localAvailable)
}

func localModel() string {
	m := os.Getenv("LOCAL_MODEL")
	if m == "" {
		m = "jarvis-brain"
	}
	return m
}

func apiModel() string {
	m := os.Getenv("API_MODEL")
	if m == "" {
		m = "claude-sonnet-5-5"
	}
	return m
}
