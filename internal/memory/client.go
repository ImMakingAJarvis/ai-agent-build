// Package memory is a client for the Jarvis Memory Service
// (scripts/memory_service.py), the local vector database of work logs.
//
// It gives the chatbot semantic search over job transcripts, worker output,
// READMEs, the kanban board and the pending queue, so it can continue
// conversations about past work and know what is up for scheduling next.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultURL is where the memory service listens on this host.
const DefaultURL = "http://127.0.0.1:8101"

// Hit is one search result chunk.
type Hit struct {
	Text   string         `json:"text"`
	Score  float64        `json:"score"`
	Source string         `json:"source"`
	Path   string         `json:"path"`
	Title  string         `json:"title"`
	Meta   map[string]any `json:"meta"`
}

// Client talks to the memory service.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for $JARVIS_MEMORY_URL (default DefaultURL) with a
// short timeout: memory is an enhancement, not a dependency.
func New() *Client {
	base := os.Getenv("JARVIS_MEMORY_URL")
	if base == "" {
		base = DefaultURL
	}
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: 3 * time.Second}}
}

// Search queries the memory service. It fails open: any error (service
// down, timeout, bad response) returns (nil, nil) so chat keeps working
// without memory.
func (c *Client) Search(ctx context.Context, query string, k int) []Hit {
	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/search", bytes.NewReader(body))
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out.Results
}

// FormatContext renders hits as a context block for an LLM system prompt.
// Each hit is one line: [source: path] text, truncated to fit maxChars.
func FormatContext(hits []Hit, maxChars int) string {
	if len(hits) == 0 || maxChars <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[source: path] excerpts\n")
	for _, h := range hits {
		label := h.Path
		if h.Source != "" && !strings.Contains(h.Path, h.Source) {
			label = h.Source + " " + h.Path
		}
		line := fmt.Sprintf("[%s] %s", label, strings.ReplaceAll(h.Text, "\n", " "))
		if b.Len()+len(line)+1 > maxChars {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
