// Package queue defines the job message schema and AMQP connection/consumer logic.
package queue

import (
	"time"
)

// Message is the canonical job shape published to and consumed from RabbitMQ.
type Message struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`       // voice, coding, background
	Priority  int               `json:"priority"`   // higher wins within a queue
	Payload   map[string]string `json:"payload"`    // free-form key/value for the job
	CreatedAt time.Time         `json:"created_at"`
	Source    string            `json:"source"`     // cli, discord, github, voice, bootstrap

	// ForceAPI, when true, skips the local model and sends straight to Claude API.
	ForceAPI bool `json:"force_api,omitempty"`

	// ModelOverride, when non-empty, overrides pickModel's routing decision.
	ModelOverride string `json:"model_override,omitempty"`

	// Kind is "" (a text job the worker runs itself) or "agent" (build work
	// run by the agent-run loop through internal/agentjob).
	Kind         string   `json:"kind,omitempty"`
	Title        string   `json:"title,omitempty"`
	Instructions string   `json:"instructions,omitempty"` // full job text incl. acceptance criteria
	Branch       string   `json:"branch,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`   // job IDs that must be done first
	Driver       string   `json:"driver,omitempty"`       // local (default) or sonnet
}

// KindAgent marks build work run with tools by a Claude Code session.
const KindAgent = "agent"

// Queue names match the RabbitMQ definitions.
const (
	VoiceQueue      = "voice"
	CodingQueue     = "coding"
	BackgroundQueue = "background"
)

// AllQueues lists the queues in priority order.
var AllQueues = []string{VoiceQueue, CodingQueue, BackgroundQueue}

// QueueForType returns the RabbitMQ queue name for a given job type.
func QueueForType(jobType string) string {
	switch jobType {
	case "voice":
		return VoiceQueue
	case "coding":
		return CodingQueue
	default:
		return BackgroundQueue
	}
}
