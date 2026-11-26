package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

const (
	fetchTimeout = 60 * time.Second
	fetchMaxBody = 1 << 20 // read at most 1 MB of body
)

// fetchClient bounds the whole request (connect through body read) at 60 s.
var fetchClient = &http.Client{Timeout: fetchTimeout}

// Fetch GETs an http(s) URL (web_fetch) and returns the status line plus
// up to 1 MB of the body. Other schemes are refused; a non-2xx response is
// a failed result with the status in Error.
type Fetch struct{}

func (Fetch) Spec() ops.Spec {
	return ops.Spec{
		Name:        "web_fetch",
		Description: "GET an http(s) URL: the status line plus up to 1 MB of body text",
		Schema: schema(map[string]any{
			"url": map[string]any{"type": "string", "description": "http or https URL"},
		}, "url"),
		Class:      ops.Normal,
		ExpectFast: false,
	}
}

func (Fetch) Run(ctx context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("web_fetch args: %w", err))
	}
	if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
		return fail(errors.New("web_fetch needs an http(s) url"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return fail(err)
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return fail(fmt.Errorf("fetch failed: %w", err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
	if err != nil {
		return fail(err)
	}
	progress()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ops.Result{Status: agentjob.OpFailed, Error: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return done(fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(b)))
}

// FetchTool returns the web_fetch tool.
func FetchTool() ops.Tool {
	return Fetch{}
}
