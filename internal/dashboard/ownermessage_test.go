package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	return New()
}

func TestOwnerMessageEndpoint(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest("POST", "/api/owner-message",
		strings.NewReader(`{"message":"  skip the benchmarks  "}`))
	w := httptest.NewRecorder()
	s.handleOwnerMessage(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := inbox.Count(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	// No job running: the response says the message waits for the next round.
	if !strings.Contains(w.Body.String(), "no job is running") {
		t.Fatalf("response = %s", w.Body.String())
	}

	// A running job is named in the response.
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: "j1", Title: "T"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("j1", agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatal(err)
	}
	if got := s.currentAgentJobID(); got != "j1" {
		t.Fatalf("currentAgentJobID = %q", got)
	}
	req2 := httptest.NewRequest("POST", "/api/owner-message",
		strings.NewReader(`{"message":"fix the tests"}`))
	w2 := httptest.NewRecorder()
	s.handleOwnerMessage(w2, req2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "j1") {
		t.Fatalf("status = %d, body %s", w2.Code, w2.Body.String())
	}

	// Empty and bad bodies are rejected and leave the inbox untouched.
	for _, body := range []string{`{"message":"   "}`, `not json`} {
		req3 := httptest.NewRequest("POST", "/api/owner-message", strings.NewReader(body))
		w3 := httptest.NewRecorder()
		s.handleOwnerMessage(w3, req3)
		if w3.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d", body, w3.Code)
		}
	}
	if got := inbox.Count(); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}

func TestAgentJobsSectionShowsWaitingMessages(t *testing.T) {
	tmpl, err := parseTemplates(t)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "agent-jobs-section",
		DashboardState{WorkerMessages: 3}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "3 message(s) waiting for the worker") {
		t.Fatalf("section lacks waiting hint:\n%s", buf.String())
	}
}
