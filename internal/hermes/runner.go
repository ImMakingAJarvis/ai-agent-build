// Package hermes (runner) provides the Hermes runner: the orchestration
// layer that sits atop the Hermes agent and wires together internet
// search, worker task delegation, and response routing.
//
// The runner is designed as a single file with interfaces for each
// capability so they can be swapped out or extended independently.
package hermes

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// RunnerConfig holds the configuration for the Hermes runner.
type RunnerConfig struct {
	// Model is the Ollama model name for intent classification
	// (e.g. "jarvis-brain").
	Model string

	// SearchEnabled enables internet search lookups.
	SearchEnabled bool

	// SearchTimeout is the maximum time to wait for a search result.
	SearchTimeout time.Duration

	// DelegationEnabled enables worker task delegation.
	DelegationEnabled bool

	// DelegationEndpoint is the HTTP endpoint for the worker queue
	// (e.g. "http://localhost:5556/api/delegate").
	DelegationEndpoint string

	// RouteRules maps result types to routing targets.
	// If empty, the default router is used.
	RouteRules map[string]string

	// LogOutput is the writer for runner logs. Defaults to os.Stdout.
	LogOutput *log.Logger
}

// Runner orchestrates the Hermes agent with search, delegation, and
// routing capabilities.
type Runner struct {
	cfg   RunnerConfig
	agent *Agent
	search SearchEngine
	delegate Delegator
	router ResponseRouter
}

// SearchEngine performs internet searches and returns result snippets.
// Implementations may use any search backend (Brave, SearXNG, etc.).
type SearchEngine interface {
	// Search queries the engine and returns up to maxResults snippets.
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
}

// SearchResult is a single search hit.
type SearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	Source      string `json:"source"`
	Confidence  float64 `json:"confidence"`
}

// Delegator hands off tasks to a worker pool.
// Implementations may push to RabbitMQ, HTTP queues, or local processes.
type Delegator interface {
	// Delegate submits a task and returns a tracking ID.
	Delegate(ctx context.Context, task DelegationTask) (string, error)

	// Status returns the current status of a delegated task.
	Status(ctx context.Context, id string) (DelegationStatus, error)
}

// DelegationTask describes a unit of work to delegate.
type DelegationTask struct {
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Priority     int               `json:"priority"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
}

// DelegationStatus tracks the state of a delegated task.
type DelegationStatus struct {
	ID       string `json:"id"`
	State    string `json:"state"` // pending, running, completed, failed
	Progress int    `json:"progress"` // 0-100
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ResponseRouter determines where a processed response should go.
// Implementations may route to Discord, HTTP clients, or local logs.
type ResponseRouter interface {
	// Route delivers a response to the appropriate destination.
	Route(ctx context.Context, dest string, response string) error

	// Destinations returns the list of registered destinations.
	Destinations() []string
}

// NewRunner creates a Runner with the given configuration.
// It initializes stub implementations for search, delegation, and routing.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.Model == "" {
		cfg.Model = "jarvis-brain"
	}
	if cfg.SearchTimeout == 0 {
		cfg.SearchTimeout = 10 * time.Second
	}
	if cfg.LogOutput == nil {
		cfg.LogOutput = log.New(os.Stdout, "[hermes-runner] ", log.LstdFlags)
	}

	r := &Runner{
		cfg:   cfg,
		agent: New(Config{Model: cfg.Model}),
	}

	// Wire up capabilities based on config.
	r.search = newStubSearch(cfg.SearchEnabled)
	r.delegate = newStubDelegator(cfg.DelegationEnabled, cfg.DelegationEndpoint)
	r.router = newStubRouter(cfg.RouteRules)

	return r
}

// Run processes a user message through the full Hermes pipeline:
// classification, optional search enrichment, optional delegation,
// and response routing.
func (r *Runner) Run(ctx context.Context, userMessage string, destination string) (string, error) {
	r.log("processing message from destination %q", destination)

	// Step 1: Classify the message.
	result, err := r.agent.Classify(ctx, userMessage)
	if err != nil {
		return "", fmt.Errorf("hermes runner: classify: %w", err)
	}

	r.log("classified as %s", result.Type)

	// Step 2: Enrich with search if the message looks like a question.
	if r.cfg.SearchEnabled && result.Type == TypeChat {
		enriched, err := r.enrichWithSearch(ctx, userMessage)
		if err != nil {
			r.log("search enrichment failed: %v", err)
		} else {
			userMessage = enriched
		}
	}

	// Step 3: Process through the agent.
	reply, err := r.agent.Process(ctx, userMessage)
	if err != nil {
		return "", fmt.Errorf("hermes runner: process: %w", err)
	}

	// Step 4: If the result is an action that should be delegated, hand it off.
	if result.Type == TypeAction && r.cfg.DelegationEnabled {
		taskID, err := r.delegate.Delegate(ctx, DelegationTask{
			Title:       result.ActionName,
			Description: userMessage,
			Priority:    5,
		})
		if err != nil {
			r.log("delegation failed: %v", err)
		} else {
			reply = fmt.Sprintf("%s\n\nTask delegated (ID: %s)", reply, taskID)
		}
	}

	// Step 5: Route the response.
	if err := r.router.Route(ctx, destination, reply); err != nil {
		r.log("routing failed: %v", err)
	}

	return reply, nil
}

// enrichWithSearch performs a search and appends results to the user message.
func (r *Runner) enrichWithSearch(ctx context.Context, query string) (string, error) {
	searchCtx, cancel := context.WithTimeout(ctx, r.cfg.SearchTimeout)
	defer cancel()

	results, err := r.search.Search(searchCtx, query, 3)
	if err != nil {
		return "", fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		return query, nil
	}

	var enriched string
	enriched += query + "\n\nSearch context:\n"
	for i, r := range results {
		enriched += fmt.Sprintf("[%d] %s — %s\n%s\n", i+1, r.Title, r.URL, r.Snippet)
	}

	r.log("enriched with %d search results", len(results))
	return enriched, nil
}

// Agent returns the underlying Hermes agent for direct access.
func (r *Runner) Agent() *Agent {
	return r.agent
}

// SearchEngine returns the configured search engine.
func (r *Runner) SearchEngine() SearchEngine {
	return r.search
}

// Delegator returns the configured delegator.
func (r *Runner) Delegator() Delegator {
	return r.delegate
}

// Router returns the configured response router.
func (r *Runner) Router() ResponseRouter {
	return r.router
}

// log writes a formatted log message.
func (r *Runner) log(format string, args ...interface{}) {
	r.cfg.LogOutput.Printf(format, args...)
}

// --- Stub implementations ---

// stubSearch is a no-op search engine. When search is disabled, it
// returns empty results; when enabled, it returns a placeholder result
// to demonstrate the pipeline works. Replace with a real implementation.
type stubSearch struct {
	enabled bool
}

func newStubSearch(enabled bool) *stubSearch {
	return &stubSearch{enabled: enabled}
}

func (s *stubSearch) Search(_ context.Context, query string, maxResults int) ([]SearchResult, error) {
	if !s.enabled {
		return nil, nil
	}
	// Stub: return a placeholder result to verify the pipeline.
	return []SearchResult{
		{
			Title:      "Stub search result for: " + query,
			URL:        "https://example.com",
			Snippet:    "(stub) internet search not yet implemented",
			Source:     "stub",
			Confidence: 0.5,
		},
	}, nil
}

// stubDelegator is a no-op delegator. Replace with a real implementation
// that pushes tasks to RabbitMQ or an HTTP queue.
type stubDelegator struct {
	enabled   bool
	endpoint  string
	nextID    int
	tasks     map[string]DelegationStatus
}

func newStubDelegator(enabled bool, endpoint string) *stubDelegator {
	return &stubDelegator{
		enabled:  enabled,
		endpoint: endpoint,
		tasks:    make(map[string]DelegationStatus),
	}
}

func (d *stubDelegator) Delegate(_ context.Context, task DelegationTask) (string, error) {
	if !d.enabled {
		return "", fmt.Errorf("delegation disabled")
	}
	d.nextID++
	id := fmt.Sprintf("hermes-task-%d", d.nextID)
	d.tasks[id] = DelegationStatus{
		ID:       id,
		State:    "pending",
		Progress: 0,
	}
	return id, nil
}

func (d *stubDelegator) Status(_ context.Context, id string) (DelegationStatus, error) {
	status, ok := d.tasks[id]
	if !ok {
		return DelegationStatus{}, fmt.Errorf("task %q not found", id)
	}
	return status, nil
}

// stubRouter is a no-op router that logs responses. Replace with a real
// implementation that sends to Discord, HTTP endpoints, or other channels.
type stubRouter struct {
	destinations []string
}

func newStubRouter(rules map[string]string) *stubRouter {
	dests := make([]string, 0, len(rules))
	for dest := range rules {
		dests = append(dests, dest)
	}
	if len(dests) == 0 {
		dests = []string{"default"}
	}
	return &stubRouter{destinations: dests}
}

func (r *stubRouter) Route(_ context.Context, dest string, response string) error {
	// Stub: log the response instead of sending it.
	log.Printf("[hermes-router] dest=%q reply=%q", dest, response)
	return nil
}

func (r *stubRouter) Destinations() []string {
	return r.destinations
}

// --- HTTP helpers ---

// NewHTTPSearch creates a SearchEngine that queries an HTTP search API.
// This is a convenience constructor; the endpoint must support a JSON
// response with a "results" array of {title, url, snippet} objects.
func NewHTTPSearch(endpoint string, timeout time.Duration) SearchEngine {
	return &httpSearch{
		endpoint: endpoint,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

type httpSearch struct {
	endpoint string
	client   *http.Client
}

func (h *httpSearch) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	// TODO: implement HTTP search query against the endpoint.
	// For now, return a stub result to indicate the extension point.
	return []SearchResult{
		{
			Title:    "HTTP search (not yet implemented)",
			URL:      h.endpoint,
			Snippet:  "query: " + query,
			Source:   "http",
			Confidence: 0.0,
		},
	}, nil
}

// NewHTTPDelegator creates a Delegator that posts tasks to an HTTP endpoint.
func NewHTTPDelegator(endpoint string, timeout time.Duration) Delegator {
	return &httpDelegator{
		endpoint: endpoint,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

type httpDelegator struct {
	endpoint string
	client   *http.Client
}

func (d *httpDelegator) Delegate(ctx context.Context, task DelegationTask) (string, error) {
	// TODO: implement HTTP POST to the delegation endpoint.
	return "http-delegate-stub", nil
}

func (d *httpDelegator) Status(ctx context.Context, id string) (DelegationStatus, error) {
	// TODO: implement HTTP GET for task status.
	return DelegationStatus{ID: id, State: "unknown"}, nil
}
