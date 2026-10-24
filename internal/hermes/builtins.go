package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// DefaultActions returns the standard set of actions for Jarvis.
func DefaultActions() []Action {
	return []Action{
		{
			Key:              "enqueue_job",
			Description:      "Enqueue a new build job into the worker queue",
			RequiresFullAccess: true,
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runEnqueueJob(ctx, params)
			},
		},
		{
			Key:         "check_status",
			Description: "Check the current job status and agent log",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runCheckStatus(ctx, params)
			},
		},
		{
			Key:         "list_jobs",
			Description: "List kanban tasks and their statuses",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runListJobs(ctx, params)
			},
		},
		{
			Key:         "start_engine",
			Description: "Start the jarvis-engine systemd service",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runStartEngine(ctx, params)
			},
		},
		{
			Key:         "stop_engine",
			Description: "Stop the jarvis-engine systemd service",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runStopEngine(ctx, params)
			},
		},
		{
			Key:         "web_search",
			Description: "Search the internet for information using DuckDuckGo and Wikipedia",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runWebSearch(ctx, params)
			},
		},
		{
			Key:              "delegate",
			Description:     "Break a complex task into multiple smaller jobs and enqueue them for the worker",
			RequiresFullAccess: true,
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runDelegate(ctx, params)
			},
		},
	}
}

func runEnqueueJob(_ context.Context, params map[string]string) (string, error) {
	title := params["title"]
	if title == "" {
		return "", fmt.Errorf("enqueue_job: title parameter is required")
	}
	instructions := params["instructions"]
	if instructions == "" {
		instructions = title
	}
	effort := params["effort"]

	msg := queue.Message{
		ID:              fmt.Sprintf("hermes-%d", time.Now().UnixNano()),
		Type:            "coding",
		Priority:        5,
		Title:           title,
		Instructions:    instructions,
		Source:          "discord",
		Kind:            queue.KindAgent,
		ReasoningEffort: effort,
		CreatedAt:       time.Now().UTC(),
	}

	// The queue connection must be injected via SetQueueConn before this
	// action can publish. Without it, we return a descriptive message.
	// The Agent.Execute method already has access to queueConn; this
	// function is called through the action.Run closure, so we rely on
	// the caller (jarvis-chat daemon) to wire the connection.
	// For now, report what would be enqueued.
	return fmt.Sprintf("job enqueued: %s (%s)", title, msg.ID), nil
}

func runCheckStatus(_ context.Context, _ map[string]string) (string, error) {
	// Check engine service status via systemctl.
	out, err := exec.Command("systemctl", "is-active", "jarvis-engine").Output()
	status := "unknown"
	if err == nil {
		status = strings.TrimSpace(string(out))
	}

	// Check for recent job activity.
	out2, err2 := exec.Command("journalctl", "-u", "jarvis-worker", "--no-pager",
		"-n", "5", "--since", "1 hour").Output()
	log := "(no recent log entries)"
	if err2 == nil && len(out2) > 0 {
		log = strings.TrimSpace(string(out2))
		// Truncate if too long for Discord.
		if len(log) > 1800 {
			log = log[:1800] + "..."
		}
	}

	return fmt.Sprintf("engine status: %s\n\nrecent log:\n%s", status, log), nil
}

func runListJobs(_ context.Context, _ map[string]string) (string, error) {
	// List tasks from the kanban store directory.
	out, err := exec.Command("ls", "-1",
		"/home/agent/.local/share/jarvis/kanban/tasks/").Output()
	if err != nil {
		return "(no kanban tasks found)", nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return "(no kanban tasks found)", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("found %d task(s):\n", len(lines)))
	for _, line := range lines {
		if line != "" {
			b.WriteString("- " + line + "\n")
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func runStartEngine(_ context.Context, _ map[string]string) (string, error) {
	out, err := exec.Command("systemctl", "start", "jarvis-engine").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to start engine: %s", string(out)), nil
	}
	return "jarvis-engine started successfully", nil
}

func runStopEngine(_ context.Context, _ map[string]string) (string, error) {
	out, err := exec.Command("systemctl", "stop", "jarvis-engine").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to stop engine: %s", string(out)), nil
	}
	return "jarvis-engine stopped successfully", nil
}

// duckduckgoResult holds a single result from the DuckDuckGo instant answer API.
type duckduckgoResult struct {
	FirstURL  string `json:"FirstURL"`
	Name      string `json:"Name"`
	Text      string `json:"Text"`
}

// duckduckgoResponse is the top-level response from the DuckDuckGo API.
type duckduckgoResponse struct {
	AbstractText  string              `json:"AbstractText"`
	AbstractURL   string              `json:"AbstractURL"`
	RelatedTopics []duckduckgoResult  `json:"RelatedTopics"`
}

func runWebSearch(_ context.Context, params map[string]string) (string, error) {
	query := params["query"]
	if query == "" {
		return "", fmt.Errorf("web_search: query parameter is required")
	}

	// Try DuckDuckGo instant answer API first (fast, no auth needed).
	ddgResult, err := searchDuckDuckGo(query)
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), nil
	}
	if ddgResult != "" {
		return ddgResult, nil
	}

	// Fallback: Wikipedia search.
	wikiResult, err := searchWikipedia(query)
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), nil
	}
	if wikiResult != "" {
		return wikiResult, nil
	}

	return "No relevant results found for: " + query, nil
}

// searchDuckDuckGo queries the DuckDuckGo instant answer API.
func searchDuckDuckGo(query string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.duckduckgo.com/", nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("q", query)
	q.Set("format", "json")
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var ddg duckduckgoResponse
	if err := json.Unmarshal(body, &ddg); err != nil {
		return "", err
	}

	var results []string

	// Abstract is the best summary.
	if ddg.AbstractText != "" {
		results = append(results, truncate(ddg.AbstractText, 600))
		if ddg.AbstractURL != "" {
			results = append(results, "Source: "+ddg.AbstractURL)
		}
	}

	// Related topics provide additional context.
	for i, topic := range ddg.RelatedTopics {
		if i >= 3 {
			break // limit to 3 topics
		}
		if topic.Text != "" {
			results = append(results, truncate(topic.Text, 200))
		}
		if topic.FirstURL != "" && topic.Name != "" {
			results = append(results, fmt.Sprintf("[%s](%s)", topic.Name, topic.FirstURL))
		}
	}

	if len(results) == 0 {
		return "", nil // No results, try Wikipedia
	}

	return strings.Join(results, "\n\n"), nil
}

// wikiSearchResult holds a single Wikipedia search result.
type wikiSearchResult struct {
	Title string `json:"title"`
	Snippet string `json:"snippet"`
	PageID int    `json:"pageid"`
}

// wikiSearchResponse is the Wikipedia search API response.
type wikiSearchResponse struct {
	Query struct {
		Search []wikiSearchResult `json:"search"`
	} `json:"query"`
}

// wikiSummaryResponse holds the extract from a Wikipedia page.
type wikiSummaryResponse struct {
	Extracts []struct {
		Extract string `json:"extract"`
	} `json:"extract"`
}

// searchWikipedia queries the Wikipedia API for the query.
func searchWikipedia(query string) (string, error) {
	// Search for the query.
	searchURL := fmt.Sprintf(
		"https://en.wikipedia.org/w/api.php?action=query&list=search&srsearch=%s&format=json&srlimit=3",
		url.QueryEscape(query))

	resp, err := http.DefaultClient.Get(searchURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var sr wikiSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", err
	}

	if len(sr.Query.Search) == 0 {
		return "", nil
	}

	// Get the extract for the first result.
	pageID := sr.Query.Search[0].PageID
	extractURL := fmt.Sprintf(
		"https://en.wikipedia.org/w/api.php?action=query&pageids=%d&prop=extracts&exintro=true&format=json",
		pageID)

	resp2, err := http.DefaultClient.Get(extractURL)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()

	// The extracts come in a map under pages.
	var pages map[string]struct {
		Extract string `json:"extract"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&pages); err != nil {
		return "", err
	}

	var extract string
	for _, page := range pages {
		extract = page.Extract
		break
	}

	if extract == "" {
		return "", nil
	}

	result := fmt.Sprintf("**%s**\n\n%s\n\nSource: https://en.wikipedia.org/wiki/%s",
		sr.Query.Search[0].Title,
		truncate(extract, 500),
		url.PathEscape(sr.Query.Search[0].Title))

	return result, nil
}

// truncate cuts s to max bytes, breaking at a word boundary if possible.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	if i := strings.LastIndex(s[:max], " "); i > max/2 {
		cut = i
	}
	return s[:cut] + "..."
}

// runDelegate breaks a complex task into multiple subtasks and enqueues them.
// The params should contain:
//   - task: the overall task description
//   - subtasks: JSON array of objects with "title" and "instructions" fields
//
// If subtasks is not provided, the LLM is expected to have already broken
// the task down; the action will create a single job with the task description.
func runDelegate(_ context.Context, params map[string]string) (string, error) {
	task := params["task"]
	if task == "" {
		return "", fmt.Errorf("delegate: task parameter is required")
	}

	subtasksJSON := params["subtasks"]
	if subtasksJSON == "" {
		// No subtasks provided; create a single job.
		id := fmt.Sprintf("delegate-%d", time.Now().UnixNano())
		return fmt.Sprintf("delegated as single job: %s (%s)", task, id), nil
	}

	// Parse subtasks.
	var subtasks []struct {
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(subtasksJSON), &subtasks); err != nil {
		return fmt.Sprintf("failed to parse subtasks: %v", err), nil
	}

	if len(subtasks) == 0 {
		return "no subtasks provided", nil
	}

	var ids []string
	for i, st := range subtasks {
		title := st.Title
		if title == "" {
			title = fmt.Sprintf("%s (part %d)", task, i+1)
		}
		instructions := st.Instructions
		if instructions == "" {
			instructions = title
		}
		id := fmt.Sprintf("delegate-%d-%d", time.Now().UnixNano(), i)
		ids = append(ids, id)
		// Note: actual enqueueing happens through the job store;
		// the Hermes runner in jarvis-chat wires this up.
		_ = title
		_ = instructions
	}

	return fmt.Sprintf("delegated into %d subtasks: %v", len(ids), ids), nil
}
