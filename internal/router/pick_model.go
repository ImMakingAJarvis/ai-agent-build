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
//   - Voice jobs → local light model (from LOCAL_LIGHT_MODEL env, defaults to
//     LOCAL_MODEL if unset) for fast response times.
//   - Fix jobs → local model (not the light model; fix tasks may need full
//     reasoning capability).
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

	// Voice jobs use the light model for fast response times.
	if jobType == "voice" {
		return localLightModel(), true
	}

	// Coding jobs use the dedicated coder model if configured.
	if jobType == "coding" {
		coder := localCoderModel()
		if coder != "" {
			return coder, true
		}
	}

	local := localModel()
	return local, true
}

// PickModelWithPayload is like PickModel but reads flags from the Message struct
// and its payload map.
func PickModelWithPayload(jobType string, forceAPI bool, modelOverride string, localAvailable bool) (modelName string, useLocal bool) {
	return PickModel(jobType, forceAPI, modelOverride, localAvailable)
}

func localModel() string {
	m := os.Getenv("LOCAL_MODEL")
	if m == "" {
		m = "jarvis-brain"
	}
	return m
}

// localLightModel returns the model for voice jobs.
// Reads LOCAL_LIGHT_MODEL; falls back to LOCAL_MODEL if unset,
// so voice routing still works when only the default model is configured.
func localLightModel() string {
	m := os.Getenv("LOCAL_LIGHT_MODEL")
	if m == "" {
		m = localModel()
	}
	return m
}

// localCoderModel returns the dedicated coding model.
// Reads LOCAL_CODER_MODEL; returns "" if unset, in which case
// the caller falls back to the general local model.
func localCoderModel() string {
	return os.Getenv("LOCAL_CODER_MODEL")
}

func apiModel() string {
	m := os.Getenv("API_MODEL")
	if m == "" {
		m = "claude-sonnet-5-5"
	}
	return m
}
