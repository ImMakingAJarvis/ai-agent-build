// Package queue defines the job message schema and AMQP connection/consumer logic.
package queue

import (
	"time"
)

// Reasoning effort levels for Claude model jobs.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// DefaultEffort is the default reasoning effort for new jobs.
const DefaultEffort = EffortMedium

// Message is the canonical job shape published to and consumed from RabbitMQ.
type Message struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`       // voice, fix, image, coding, background
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
	Kind string `json:"kind,omitempty"`

	// Title is a short human-readable label for the job.
	Title string `json:"title,omitempty"`

	// Instructions is the full job text including acceptance criteria.
	Instructions string `json:"instructions,omitempty"`

	// Branch is the git branch to work on.
	Branch string `json:"branch,omitempty"`

	// DependsOn lists job IDs that must be done before this one starts.
	DependsOn []string `json:"depends_on,omitempty"`

	// Driver is "local" (default) or "sonnet".
	Driver string `json:"driver,omitempty"`

	// Class is the agent-job priority class: "fix", "build" or
	// "background" (see agentjob.Class*). Fix jobs always run before build
	// jobs, which always run before background ones. Empty means build
	// (background when Source is "migrate").
	Class string `json:"class,omitempty"`

	// ReasoningEffort controls thinking depth: off, low, medium, high, xhigh, max.
	// Voice defaults to off; coding defaults to medium. "unknown" is not valid
	// (Claude Code sends it for unrecognized model IDs).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// KindAgent marks build work run with tools by a Claude Code session.
const KindAgent = "agent"

// Queue names match the RabbitMQ definitions.
const (
	VoiceQueue      = "voice"
	FixQueue        = "fix"
	ImageQueue      = "image"
	CodingQueue     = "coding"
	BackgroundQueue = "background"
)

// AllQueues lists the queues in priority order (highest first). Image
// (photo generation) sits above coding and background — it almost always
// takes priority, waiting only behind voice and fix.
var AllQueues = []string{VoiceQueue, FixQueue, ImageQueue, CodingQueue, BackgroundQueue}

// PlanEpicPriority is the AMQP priority of plan_epic jobs published to the
// background queue (B1): the maximum the background queue accepts
// (x-max-priority 10), so a planning job sits at the front of the background
// queue. It only orders pending deliveries — it never preempts a running
// job — and the scheduler still keeps the conversation queues (voice, fix,
// coding) ahead of the background queue.
const PlanEpicPriority = 10

// QueueForType returns the RabbitMQ queue name for a given job type.
func QueueForType(jobType string) string {
	switch jobType {
	case "voice":
		return VoiceQueue
	case "fix":
		return FixQueue
	case "image":
		return ImageQueue
	case "coding":
		return CodingQueue
	default:
		return BackgroundQueue
	}
}
