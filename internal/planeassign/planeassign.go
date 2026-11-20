// Package planeassign maps Plane workspace members to registry model ids.
// The board picks a model by assignee, not by a plan: or model: label.
package planeassign

import "strings"

// QwenEmail is the Plane user that runs the current local model
// (qwen3.8-27b). Planned tasks are assigned to this user and set Ready.
const QwenEmail = "jarvis-bot2@nathan.homes"

// user is one Plane member and the registry model that member stands for.
type user struct {
	email   string
	modelID string
}

// users is the assignee map Nathan set up on the jarvis workspace.
// jarvis-big-brain is the future MoE planner; that registry entry stays
// disabled until the model is pulled, so assigning it does not run a job.
var users = []user{
	{QwenEmail, "qwen3.8-27b"},
	{"jarvis-big-brain@nathan.homes", "qwen3.8-flash-next"},
	{"fable-bot@nathan.homes", "claude-fable-5-1"},
	{"sonnet-bot@nathan.homes", "claude-sonnet-5-5"},
	{"haiku-bot@nathan.homes", "claude-haiku-5-5"},
	{"opus-bot@nathan.homes", "claude-opus-5-5"},
}

// ModelForEmail returns the registry id for a Plane member email.
func ModelForEmail(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, u := range users {
		if u.email == email {
			return u.modelID, true
		}
	}
	return "", false
}
