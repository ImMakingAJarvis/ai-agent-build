// Package queue defines the job message schema and AMQP connection/consumer logic.
package queue

import (
	"path/filepath"
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

	// Driver is "local" (default) or "sonnet" / "opus" / "superagent"
	// (or "igpu" for the iGPU helper).
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

	// --- Envelope v2 (event-driven capabilities, plan §3.3) --------------
	// Capability jobs (image.generate, audio.transcribe, ...) carry these;
	// the v1 job fields above do not. All are omitempty: a v1 message (none
	// set) encodes and decodes byte-for-byte as before, so old publishers
	// and consumers interoperate. CorrelationID and ReplyTo are also mirrored
	// onto the native AMQP properties (CorrelationID/ReplyTo) at publish time
	// (see newPublishing), so a CLI, a Python worker, or the broker's own
	// logs see the same values the JSON body carries.

	// Capability is the thing being asked for, e.g. "image.generate". One
	// capability maps to one cap.<capability> queue.
	Capability string `json:"capability,omitempty"`

	// CorrelationID ties a request to its Result. Handlers are idempotent
	// by this value (see DoneMarker). Also AMQP correlation_id.
	CorrelationID string `json:"correlation_id,omitempty"`

	// ReplyTo is the flow queue the Result is published to, e.g.
	// "flow.<job-id>". Also AMQP reply_to.
	ReplyTo string `json:"reply_to,omitempty"`

	// WakeJob is the agentjob id to wake when the result lands. Redundant
	// with the switchboard listener, but lets a worker wake the job directly
	// if the switchboard is down.
	WakeJob string `json:"wake_job,omitempty"`

	// ParentJob is the id of the job that submitted this one, for tracing
	// chains of work.
	ParentJob string `json:"parent_job,omitempty"`

	// Input holds capability-specific arguments (a free-form JSON object).
	Input map[string]any `json:"input,omitempty"`

	// InputRefs are paths (files or directories) the worker reads. Paths
	// only — never the bytes; large payloads stay in the artifacts root and
	// are referenced, not inlined.
	InputRefs []string `json:"input_refs,omitempty"`

	// OutDir is where the worker writes its artifacts, e.g.
	// ~/.local/share/jarvis/artifacts/<job-id>/.
	OutDir string `json:"out_dir,omitempty"`

	// Location pins the work to a device: "gpu", "igpu", "cpu", or
	// "remote:<host>". Empty means the broker routes to the best free worker
	// (consumer priority).
	Location string `json:"location,omitempty"`

	// Resources hints how much memory the capability needs. nil means the
	// worker uses its own defaults.
	Resources *Resources `json:"resources,omitempty"`

	// Deadline is when the requester gives up; the switchboard wakes the
	// job with status=timeout at or after it.
	Deadline *time.Time `json:"deadline,omitempty"`

	// Attempt is the delivery attempt number (0 = first).
	Attempt int `json:"attempt,omitempty"`

	// SchemaVersion is the envelope version; 2 for capability jobs. 0
	// (absent) means a v1 message.
	SchemaVersion int `json:"v,omitempty"`
}

// KindAgent marks build work run with tools by a Claude Code session.
const KindAgent = "agent"

// Resources hints how much memory a capability needs on its target device.
// A zero field (or a nil *Resources) means "use the worker's own default":
// the broker's co-scheduling policy fits workers into whatever is actually
// free (plan §3.6), so these are hints, not hard requirements.
type Resources struct {
	// VRAMMB is the requested GPU memory in megabytes.
	VRAMMB int `json:"vram_mb,omitempty"`
	// RAMMB is the requested system memory in megabytes.
	RAMMB int `json:"ram_mb,omitempty"`
}

// Result status values. A Result is published to the request's ReplyTo
// queue carrying the same CorrelationID as the request.
const (
	ResultOK        = "ok"
	ResultError     = "error"
	ResultTimeout   = "timeout"
	ResultCancelled = "cancelled"
)

// Result is the reply a capability worker publishes to a request's ReplyTo
// queue. It is a reference, not a payload (plan invariant 7, "results are
// references"): Artifacts are paths under the artifacts root, never the
// bytes themselves, and Data holds small structured output only (a caption,
// a transcript path, scores) — anything large goes in an artifact file and
// is referenced here.
type Result struct {
	// CorrelationID matches the request's CorrelationID.
	CorrelationID string `json:"correlation_id"`
	// Capability is the capability that produced this result.
	Capability string `json:"capability"`
	// Status is one of ResultOK, ResultError, ResultTimeout, ResultCancelled.
	Status string `json:"status"`
	// Artifacts are paths to the outputs (under the artifacts root).
	Artifacts []string `json:"artifacts"`
	// Summary is the short text the brain reads first.
	Summary string `json:"summary"`
	// Data is small structured output (captions, a transcript path, scores).
	Data map[string]any `json:"data,omitempty"`
	// Worker identifies the worker that ran the job, e.g.
	// "image.generate@gpu/jarvis-art".
	Worker string `json:"worker"`
	// Model is the model that produced the result.
	Model string `json:"model"`
	// StartedAt and FinishedAt bracket the run.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Error is set when Status is ResultError (or ResultTimeout).
	Error string `json:"error,omitempty"`
}

// DoneMarker returns the path of the idempotency marker for a request:
// <out_dir>/.done-<correlation_id>. A capability handler must check for this
// file before doing work — if it exists, the request already finished (this
// is a redelivery), and the handler must not redo it (plan invariant 4,
// "idempotent by correlation_id"). The capworker SDK (EVB-19) writes the
// marker after a successful run and checks it before one; this function
// fixes the path convention so every worker and every test agrees on it.
func DoneMarker(outDir, correlationID string) string {
	return filepath.Join(outDir, ".done-"+correlationID)
}

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
