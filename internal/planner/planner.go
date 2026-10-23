// Package planner provides an LLM-powered task analyzer that breaks a
// high-level work description into discrete, difficulty-scored chunks
// suitable for enqueueing as agent jobs.
//
// The planner sends the task description to the local model, which returns
// a structured JSON plan with individual work chunks. Each chunk has a
// difficulty score (1-5) that influences which model/driver handles it:
//
//	1 - Simple, well-defined task (local, low effort)
//	2 - Straightforward with some setup (local, medium effort)
//	3 - Moderate complexity, needs reasoning (local, high effort)
//	4 - Complex, requires deep thinking (sonnet, high effort)
//	5 - Very complex, multi-faceted (sonnet, max effort)
package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	// Difficulty levels.
	DifficultyTrivial = 1
	DifficultyEasy    = 2
	DifficultyMedium  = 3
	DifficultyHard    = 4
	DifficultyExpert  = 5
)

// Plan is the structured output from the LLM analyzer.
type Plan struct {
	Title       string  `json:"title"`
	Summary     string  `json:"summary"`
	TotalEst    string  `json:"total_est,omitempty"` // rough time estimate
	Chunks      []Chunk `json:"chunks"`
	Constraints string  `json:"constraints,omitempty"` // notes about ordering, risks, etc.
}

// Chunk is a discrete unit of work within a plan.
type Chunk struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
	Difficulty   int    `json:"difficulty"` // 1-5
	Type         string `json:"type"`       // coding, fix, config, testing, research
	Priority     int    `json:"priority"`   // 1-10, higher = more urgent
	DependsOn    []string `json:"depends_on,omitempty"` // chunk IDs this chunk depends on
	Branch       string `json:"branch,omitempty"`       // suggested git branch name
	Estimate     string `json:"estimate,omitempty"`     // rough time estimate for this chunk
}

// Planner analyzes work descriptions and breaks them into chunks.
type Planner struct {
	client  *models.Client
	model   string
	store   *agentjob.Store
	queue   *queue.Conn
	source  string // origin label (e.g. "planner", "discord")
}

// Config holds configuration for the planner.
type Config struct {
	Model  string          // Ollama model name (default: from LOCAL_MODEL env)
	Store  *agentjob.Store // agent job store
	Queue  *queue.Conn     // RabbitMQ connection (optional; without it, chunks are stored but not published)
	Source string          // origin label
}

// New creates a new Planner.
func New(cfg Config) *Planner {
	model := cfg.Model
	if model == "" {
		model = "jarvis-brain"
		if m := modelsEnv("LOCAL_MODEL"); m != "" {
			model = m
		}
	}
	return &Planner{
		client: models.NewClient(),
		model:  model,
		store:  cfg.Store,
		queue:  cfg.Queue,
		source: cfg.Source,
	}
}

func modelsEnv(key string) string {
	return os.Getenv(key)
}

// PlanTask sends the task description to the LLM and returns a structured plan.
func (p *Planner) PlanTask(taskDescription string) (*Plan, error) {
	prompt := buildPlanPrompt(taskDescription)
	raw, err := p.client.Chat(p.model, []models.ChatMessage{
		{Role: "system", Content: planSystemPrompt},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return nil, fmt.Errorf("planner: LLM call failed: %w", err)
	}

	plan, err := parsePlan(raw)
	if err != nil {
		return nil, fmt.Errorf("planner: parse plan: %w", err)
	}
	return plan, nil
}

// EnqueuePlan takes a plan and enqueues each chunk as an agent job.
// It maps difficulty to driver and reasoning effort, sets up dependencies,
// and publishes to the queue. Returns the job IDs in order.
func (p *Planner) EnqueuePlan(plan *Plan) ([]string, error) {
	if p.store == nil {
		return nil, fmt.Errorf("planner: no job store configured")
	}

	jobIDs := make([]string, 0, len(plan.Chunks))
	now := time.Now().UTC()

	// Pre-compute the chunk-ID → job-ID mapping so dependencies can be resolved.
	chunkIDToJobID := make(map[string]string, len(plan.Chunks))
	for i, chunk := range plan.Chunks {
		chunkID := fmt.Sprintf("%s-chunk-%d", strings.ToLower(
			strings.ReplaceAll(strings.ReplaceAll(plan.Title, " ", "-"), "-", "-")), i+1)
		chunkID = strings.ReplaceAll(chunkID, "--", "-")
		chunkID = strings.Trim(chunkID, "-")
		chunkID = fmt.Sprintf("plan-%d-%s", now.Unix(), chunkID)
		chunkIDToJobID[chunk.ID] = chunkID
	}

	for i, chunk := range plan.Chunks {
		// Build a stable ID from the plan title and chunk index.
		chunkID := fmt.Sprintf("%s-chunk-%d", strings.ToLower(
			strings.ReplaceAll(strings.ReplaceAll(plan.Title, " ", "-"), "-", "-")), i+1)
		// Sanitize: remove consecutive dashes and leading/trailing dashes.
		chunkID = strings.ReplaceAll(chunkID, "--", "-")
		chunkID = strings.Trim(chunkID, "-")
		chunkID = fmt.Sprintf("plan-%d-%s", now.Unix(), chunkID)

		// Map difficulty to driver and effort.
		driver, effort := DifficultyToDriver(chunk.Difficulty)

		// Build instructions with context.
		instructions := buildChunkInstructions(plan, chunk)

		// Determine priority: base priority from chunk, adjusted by difficulty.
		priority := chunk.Priority
		if priority == 0 {
			priority = 5
		}

		// Map chunk type to job type.
		jobType := mapChunkType(chunk.Type)

		// Resolve dependency chunk IDs to actual job IDs.
		jobDeps := make([]string, 0, len(chunk.DependsOn))
		for _, dep := range chunk.DependsOn {
			if jid, ok := chunkIDToJobID[dep]; ok {
				jobDeps = append(jobDeps, jid)
			}
		}

		spec := agentjob.Spec{
			ID:             chunkID,
			Title:          fmt.Sprintf("[%s] %s", plan.Title, chunk.Title),
			Instructions:   instructions,
			Branch:         chunk.Branch,
			DependsOn:      jobDeps,
			Driver:         driver,
			Priority:       priority,
			CreatedAt:      now,
			Source:         p.source,
			ReasoningEffort: effort,
		}

		added, err := p.store.Ingest(spec)
		if err != nil {
			return jobIDs, fmt.Errorf("ingest chunk %d: %w", i, err)
		}
		if !added {
			jobIDs = append(jobIDs, chunkID)
			continue
		}

		// If we have a queue connection, also publish to RabbitMQ.
		if p.queue != nil {
			msg := queue.Message{
				ID:              chunkID,
				Type:            jobType,
				Priority:        priority,
				Title:           spec.Title,
				Instructions:    instructions,
				Branch:          chunk.Branch,
				DependsOn:       jobDeps,
				Driver:          driver,
				Source:          p.source,
				Kind:            queue.KindAgent,
				ReasoningEffort: effort,
				CreatedAt:       now,
			}
			routingKey := jobType + ".default"
			if err := p.queue.Publish(msg, routingKey); err != nil {
				return jobIDs, fmt.Errorf("publish chunk %d: %w", i, err)
			}
		}

		jobIDs = append(jobIDs, chunkID)
	}

	return jobIDs, nil
}

// DifficultyToDriver maps a difficulty score (1-5) to a driver and
// reasoning effort level.
func DifficultyToDriver(difficulty int) (driver, effort string) {
	switch {
	case difficulty <= 2:
		return "local", queue.EffortLow
	case difficulty == 3:
		return "local", queue.EffortHigh
	case difficulty == 4:
		return "sonnet", queue.EffortHigh
	default: // 5+
		return "sonnet", queue.EffortMax
	}
}

// mapChunkType maps a chunk type to a RabbitMQ job type.
func mapChunkType(chunkType string) string {
	switch strings.ToLower(chunkType) {
	case "fix", "bug", "bugfix":
		return queue.FixQueue
	case "coding", "feature", "implementation", "code":
		return queue.CodingQueue
	case "testing", "test":
		return queue.CodingQueue
	case "config", "configuration":
		return queue.BackgroundQueue
	case "research", "analysis":
		return queue.BackgroundQueue
	default:
		return queue.CodingQueue
	}
}

// buildChunkInstructions assembles the full instructions for a chunk,
// including the plan summary and constraints for context.
func buildChunkInstructions(plan *Plan, chunk Chunk) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Task: %s\n\n", chunk.Title))
	b.WriteString(fmt.Sprintf("## Difficulty: %d/5\n\n", chunk.Difficulty))
	if plan.Summary != "" {
		b.WriteString("## Plan Summary\n")
		b.WriteString(plan.Summary)
		b.WriteString("\n\n")
	}
	if plan.Constraints != "" {
		b.WriteString("## Constraints & Notes\n")
		b.WriteString(plan.Constraints)
		b.WriteString("\n\n")
	}
	b.WriteString("## Instructions\n")
	b.WriteString(chunk.Instructions)
	if chunk.Estimate != "" {
		b.WriteString(fmt.Sprintf("\n\n## Estimated Time\n%s", chunk.Estimate))
	}
	return b.String()
}

// parsePlan extracts a Plan from the LLM response.
func parsePlan(raw string) (*Plan, error) {
	// The LLM may wrap JSON in markdown code blocks.
	jsonStr := extractJSON(raw)

	var plan Plan
	if err := json.Unmarshal(jsonStr, &plan); err != nil {
		return nil, fmt.Errorf("invalid JSON plan: %w\nraw response: %s", err, truncate(raw, 500))
	}
	if len(plan.Chunks) == 0 {
		return nil, fmt.Errorf("plan has no chunks")
	}
	return &plan, nil
}

// extractJSON pulls a JSON object from text that may contain markdown
// code fences or surrounding prose.
func extractJSON(s string) []byte {
	// Try the whole string first.
	trimmed := []byte(strings.TrimSpace(s))
	if bytes, ok := tryJSON(trimmed); ok {
		return bytes
	}

	// Look for JSON between { and }.
	start := strings.Index(s, "{")
	if start == -1 {
		return trimmed
	}
	end := strings.LastIndex(s, "}")
	if end == -1 || end < start {
		return trimmed
	}
	candidate := []byte(s[start : end+1])
	if bytes, ok := tryJSON(candidate); ok {
		return bytes
	}
	return trimmed
}

func tryJSON(b []byte) ([]byte, bool) {
	var v interface{}
	if json.Unmarshal(b, &v) == nil {
		return b, true
	}
	return nil, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- LLM Prompts ---

const planSystemPrompt = `You are a task planning assistant for an AI agent build system.
Your job is to analyze a work description and break it into discrete, well-scoped chunks of work.

Respond with ONLY a JSON object matching this schema. No other text, no markdown fences:

{
  "title": "Short title for the overall task",
  "summary": "One or two sentences summarizing what needs to be done",
  "total_est": "Rough total time estimate",
  "chunks": [
    {
      "id": "chunk-1",
      "title": "Short title for this chunk",
      "instructions": "Detailed step-by-step instructions for this chunk of work. Be specific about what files to create/modify, what to test, and acceptance criteria.",
      "difficulty": 3,
      "type": "coding",
      "priority": 5,
      "depends_on": [],
      "branch": "suggested-git-branch-name",
      "estimate": "15 min"
    }
  ],
  "constraints": "Notes about ordering, risks, or dependencies between chunks"
}

Difficulty scoring:
  1 - Simple, well-defined task (copy/paste, config change, simple fix)
  2 - Straightforward with some setup or minor research
  3 - Moderate complexity, needs reasoning and design decisions
  4 - Complex, requires deep thinking, architecture decisions, or debugging
  5 - Very complex, multi-faceted, high risk of getting it wrong

Chunk types: coding, fix, config, testing, research

Rules:
- Each chunk should be small enough to complete in one agent session (under 30 minutes of work).
- Instructions must be specific: name files, describe functions, list acceptance criteria.
- Set depends_on to list chunk IDs that must complete before this chunk starts.
- Order chunks logically: foundations first, then features, then testing.
- If a task is already small (one chunk), still return it as a single-chunk plan.`

func buildPlanPrompt(taskDescription string) string {
	return fmt.Sprintf("Analyze the following work description and break it into work chunks:\n\n%s", taskDescription)
}
