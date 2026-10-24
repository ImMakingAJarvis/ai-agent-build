package dashboard

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestParseTranscriptJSON(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Check the store"},{"type":"tool_use","name":"Bash","input":{"command":"ls jobs"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"d0.1\n"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Listed the jobs."}]}}`,
	}, "\n")
	ev := ParseTranscript(raw)
	if len(ev) != 4 {
		t.Fatalf("events=%d %+v", len(ev), ev)
	}
	if ev[0].Kind != "thought" || !strings.Contains(ev[0].Body, "store") {
		t.Errorf("thought: %+v", ev[0])
	}
	if ev[1].Kind != "command" || ev[1].Title != "Bash" || ev[1].Body != "ls jobs" {
		t.Errorf("command: %+v", ev[1])
	}
	if ev[2].Kind != "result" || !strings.Contains(ev[2].Body, "d0.1") {
		t.Errorf("result: %+v", ev[2])
	}
	if ev[3].Kind != "output" || !strings.Contains(ev[3].Body, "Listed") {
		t.Errorf("output: %+v", ev[3])
	}
}

func TestParseTranscriptText(t *testing.T) {
	raw := "Thinking: look at files\n● Bash(ls -la)\n  ⎿ ok\nDone."
	ev := ParseTranscript(raw)
	if len(ev) < 3 {
		t.Fatalf("events=%d %+v", len(ev), ev)
	}
	if ev[0].Kind != "thought" {
		t.Errorf("first %+v", ev[0])
	}
	if ev[1].Kind != "command" || ev[1].Body != "ls -la" {
		t.Errorf("cmd %+v", ev[1])
	}
}

func TestParseTranscriptEmpty(t *testing.T) {
	if ParseTranscript("  \n") != nil {
		t.Fatal("expected nil")
	}
}

func TestJobLogPages(t *testing.T) {
	dir := t.TempDir()
	st, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: "d0.1", Title: "Show logs"}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFile("d0.1", "transcript.txt", []byte("Thinking: plan\n● Bash(echo hi)\n  ⎿ hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: "empty-job", Title: "Never ran"}); err != nil {
		t.Fatal(err)
	}
	hist, _ := json.Marshal(JobRecord{ID: "hist-only", Type: "background", Title: "Worker reply", Status: "completed", Summary: "just a summary"})
	if err := os.WriteFile(filepath.Join(dir, "jobs.jsonl"), append(hist, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jobs/{id}", s.handleJobLog)
	mux.HandleFunc("GET /html/jobs/{id}", s.handleJobLogHTML)

	t.Run("missing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/no-such-job", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "No job with this ID is on disk") {
			t.Fatalf("body: %s", rec.Body.String())
		}
	})

	t.Run("empty", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/html/jobs/empty-job", nil))
		if rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "No log for this job") {
			t.Fatalf("body: %s", rec.Body.String())
		}
	})

	t.Run("transcript", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/d0.1", nil))
		if rec.Code != 200 {
			t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, w := range []string{"d0.1 — Show logs", "Loading this job's log", `hx-get="/html/jobs/d0.1"`} {
			if !strings.Contains(body, w) {
				t.Errorf("page lacks %q", w)
			}
		}
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/html/jobs/d0.1", nil))
		body = rec.Body.String()
		for _, w := range []string{"Thought", "Bash", "echo hi", "Outcome"} {
			if !strings.Contains(body, w) {
				t.Errorf("fragment lacks %q in %s", w, body)
			}
		}
	})

	t.Run("history summary", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/html/jobs/hist-only", nil))
		if rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "just a summary") {
			t.Fatalf("body: %s", rec.Body.String())
		}
	})

	t.Run("bad id", func(t *testing.T) {
		page := s.loadJobLog("..")
		if !page.Missing {
			t.Fatalf("expected missing, got %+v", page)
		}
	})
}

func TestDashboardContentHasLogLinks(t *testing.T) {
	tmpl, err := template.New("dashboard-content").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	st := DashboardState{
		AgentJobs: []agentjob.Job{{
			Spec:  agentjob.Spec{ID: "d0", Title: "Fix poll"},
			State: agentjob.State{Status: agentjob.Done},
		}},
		RecentJobs: []JobRecord{{ID: "cli-1", Status: "completed"}},
	}
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "dashboard-content", st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{`href="/jobs/d0"`, `href="/jobs/cli-1"`, ">Log<"} {
		if !strings.Contains(out, w) {
			t.Errorf("content lacks %q", w)
		}
	}
}
