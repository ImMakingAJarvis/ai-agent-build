package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

func runFetch(t *testing.T, args map[string]any) ops.Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return FetchTool().Run(context.Background(), ops.Call{Args: b}, func() {})
}

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello body")
	}))
	defer srv.Close()
	res := runFetch(t, map[string]any{"url": srv.URL})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if !strings.HasPrefix(res.Output, "HTTP 200") || !strings.Contains(res.Output, "hello body") {
		t.Errorf("output %q", res.Output)
	}
}

func TestFetch404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	res := runFetch(t, map[string]any{"url": srv.URL})
	if res.Status != agentjob.OpFailed {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "404") {
		t.Errorf("error %q, want the status in it", res.Error)
	}
}

func TestFetchRejectsFileScheme(t *testing.T) {
	res := runFetch(t, map[string]any{"url": "file:///etc/passwd"})
	if res.Status != agentjob.OpFailed || !strings.Contains(res.Error, "http(s)") {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
}
