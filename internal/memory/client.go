// Package memory is a client for the Jarvis Memory Service — the local
// vector database over the work logs, job history, pending queue and repo
// docs (scripts/memory_service.py, port 8101). The chatbot uses it to look
// up what work happened and what is up for scheduling next.
//
// The client fails open: Search returns nil when the service is down or
// errors, so chat keeps working without the memory service.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultBaseURL is used when MEMORY_URL is unset (native unit, tests).
const defaultBaseURL = "http://localhost:8101"

// Hit is one search result from the memory service.
type Hit struct {
	Text   string  `json:"text"`
	Score  float64 `json:"score"`
	Source string  `json:"source"` // job, jobs, kanban, queue, repo-doc
	Path   string  `json:"path"`   // file path or doc id within the source
	Title  string  `json:"title"`
}

// Client talks to the memory service over HTTP.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for baseURL; empty baseURL means defaultBaseURL.
func NewClient(baseURL string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Search runs a semantic query and returns up to k hits, most relevant
// first. It fails open: any error (service down, bad response) yields nil.
func (c *Client) Search(ctx context.Context, query string, k int) []Hit {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if k <= 0 {
		k = 5
	}
	if c == nil || c.HTTP == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var out struct {
		Results []Hit `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil
	}
	return out.Results
}

// FormatContext renders hits as a compact context block for a system
// prompt: one header line, then "[source path] text" per hit, as many as
// fit in budget characters. Returns "" when there are no hits or the
// budget cannot hold the header.
func FormatContext(hits []Hit, budget int) string {
	const header = "[source: path] excerpts"
	if len(hits) == 0 || budget < len(header) {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for _, h := range hits {
		line := fmt.Sprintf("[%s %s] %s", h.Source, h.Path, h.Text)
		if b.Len()+1+len(line) > budget {
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}
