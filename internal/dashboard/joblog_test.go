package dashboard

import (
	"encoding/json"
	"fmt"
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

// TestParseTranscriptStreamEvents covers --include-partial-messages output,
// where every content_block_* event arrives wrapped in a stream_event
// envelope and the full assistant messages repeat the streamed content.
func TestParseTranscriptStreamEvents(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check the"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" store"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Bash"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls jobs\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"check the store"},{"type":"tool_use","name":"Bash","input":{"command":"ls jobs"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"d0.1\n"}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Listed the "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"jobs."}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"result","subtype":"success","result":"Listed the jobs."}`,
	}, "\n")
	ev := ParseTranscript(raw)
	if len(ev) != 5 {
		t.Fatalf("events=%d %+v", len(ev), ev)
	}
	if ev[0].Kind != "thought" || ev[0].Body != "check the store" {
		t.Errorf("thought: %+v", ev[0])
	}
	if ev[1].Kind != "command" || ev[1].Title != "Bash" || ev[1].Body != "ls jobs" {
		t.Errorf("command: %+v", ev[1])
	}
	if ev[2].Kind != "result" || !strings.Contains(ev[2].Body, "d0.1") {
		t.Errorf("result: %+v", ev[2])
	}
	if ev[3].Kind != "output" || ev[3].Title != "Reply" || ev[3].Body != "Listed the jobs." {
		t.Errorf("reply: %+v", ev[3])
	}
	if ev[4].Kind != "output" || ev[4].Title != "Result" || ev[4].Body != "Listed the jobs." {
		t.Errorf("final result: %+v", ev[4])
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

	tmpl, err := template.New("").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl"), tmpl: tmpl}
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
		for _, w := range []string{"d0.1 — Show logs", "/api/jobs/d0.1/log/stream"} {
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

func TestSectionsHaveLogLinks(t *testing.T) {
	tmpl, err := template.New("").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
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
	st.InProgress, st.Waiting, st.WaitingMore, st.Finished, st.FinishedTotal = splitAgentJobs(st.AgentJobs)
	var buf strings.Builder
	for _, name := range []string{"agent-jobs-section", "jobs-section"} {
		if err := tmpl.ExecuteTemplate(&buf, name, st); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	for _, w := range []string{`href="/jobs/d0"`, `href="/jobs/cli-1"`, ">Log<"} {
		if !strings.Contains(out, w) {
			t.Errorf("content lacks %q", w)
		}
	}
}

func TestLogStreamTail(t *testing.T) {
	dir := t.TempDir()
	st, _ := agentjob.Open(dir)
	if _, err := st.Ingest(agentjob.Spec{ID: "big"}); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("line of log output number x\n")
	}
	if err := st.WriteFile("big", "transcript.txt", []byte(sb.String())); err != nil {
		t.Fatal(err)
	}
	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs/{id}/log/stream", s.handleJobLogStream)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/big/log/stream", nil))
	if rec.Header().Get("X-Log-Truncated") != "true" || rec.Body.Len() > defaultLogTail {
		t.Fatalf("expected truncated tail, got %d bytes, header %q", rec.Body.Len(), rec.Header().Get("X-Log-Truncated"))
	}
	if !strings.HasPrefix(rec.Body.String(), "line of log") {
		t.Fatalf("partial first line not dropped: %.30q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/big/log/stream?tail=0", nil))
	if rec.Body.Len() != sb.Len() {
		t.Fatalf("full stream %d != %d", rec.Body.Len(), sb.Len())
	}
}

func TestLogStreamEventsMode(t *testing.T) {
	dir := t.TempDir()
	st, _ := agentjob.Open(dir)
	if _, err := st.Ingest(agentjob.Spec{ID: "big"}); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("line of log output number x\n")
	}
	if err := st.WriteFile("big", "transcript.txt", []byte(sb.String())); err != nil {
		t.Fatal(err)
	}
	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs/{id}/log/stream", s.handleJobLogStream)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/big/log/stream?format=events", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if rec.Header().Get("X-Log-Truncated") != "true" || rec.Header().Get("X-Log-Size") == "" {
		t.Fatalf("headers %q / %q", rec.Header().Get("X-Log-Truncated"), rec.Header().Get("X-Log-Size"))
	}
	var out struct {
		Events []LogEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(out.Events) == 0 || out.Events[0].Kind != "output" {
		t.Fatalf("events: %+v", out.Events)
	}

	// tail=0 returns the whole log parsed, not truncated.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/big/log/stream?tail=0&format=events", nil))
	if rec.Header().Get("X-Log-Truncated") != "false" {
		t.Fatalf("full stream should not be truncated, header %q", rec.Header().Get("X-Log-Truncated"))
	}
	var full struct {
		Events []LogEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(full.Events) == 0 {
		t.Fatal("no events for full log")
	}
}

func TestIndexIsStaticShell(t *testing.T) {
	tmpl, err := template.New("").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tmpl: tmpl} // no backends: index must not touch them
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	for _, w := range []string{`hx-get="/html/lightweight"`, `hx-get="/html/jobs-section"`, `hx-get="/html/agent-jobs-section"`} {
		if !strings.Contains(body, w) {
			t.Errorf("index lacks %q", w)
		}
	}
	// The interactive queue ships in the static shell: a container the JS
	// fills, a refresh button, and the Pointer-Event drag script.
	for _, w := range []string{`id="queue-content"`, `id="queue-refresh-btn"`, `onclick="qLoad(true)"`, `qOnDown(e, this)`, `/api/queue/reorder`, `fetch('/api/queue')`} {
		if !strings.Contains(body, w) {
			t.Errorf("index lacks queue markup %q", w)
		}
	}
}

func TestSplitAgentJobs(t *testing.T) {
	var jobs []agentjob.Job
	mk := func(id, status, fin string) {
		jobs = append(jobs, agentjob.Job{Spec: agentjob.Spec{ID: id}, State: agentjob.State{Status: status, FinishedAt: fin}})
	}
	mk("run", "running", "")
	for i := 0; i < panelWaitingMax+3; i++ {
		mk(fmt.Sprintf("r%d", i), "ready", "")
	}
	for i := 0; i < panelFinishedMax+5; i++ {
		mk(fmt.Sprintf("d%02d", i), "done", fmt.Sprintf("2026-10-27T10:%02d:00Z", i))
	}
	mk("bad", "failed", "2026-10-28T00:00:00Z")
	ip, w, more, fin, total := splitAgentJobs(jobs)
	if len(ip) != 1 || ip[0].ID != "run" {
		t.Errorf("in progress: %+v", ip)
	}
	if len(w) != panelWaitingMax || more != 3 {
		t.Errorf("waiting %d more %d", len(w), more)
	}
	if len(fin) != panelFinishedMax || total != panelFinishedMax+6 || fin[0].ID != "bad" {
		t.Errorf("finished %d total %d first %s", len(fin), total, fin[0].ID)
	}
}
