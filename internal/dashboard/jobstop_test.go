package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestJobStopEndpoint(t *testing.T) {
	s := newTestServer(t)
	conf := t.TempDir()
	t.Setenv("JARVIS_CONF", conf)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/jobs/{id}/stop", s.handleJobStop)
	post := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		return rec
	}

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

	// Stopping a running job writes the signal the runner's watcher polls
	// and parks the job (no watcher in the test: the direct fallback acts).
	w := post("/api/jobs/j1/stop")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "parked") {
		t.Fatalf("response = %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(conf, "stop-signal")); err != nil {
		t.Fatal("stop-signal should be written")
	}
	if got, _ := st.Get("j1"); got.Status != agentjob.Parked {
		t.Fatalf("want parked, got %s", got.Status)
	}

	// Stopping a parked job is an informational no-op.
	if w2 := post("/api/jobs/j1/stop"); w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "Already parked") {
		t.Fatalf("status = %d, body %s", w2.Code, w2.Body.String())
	}

	// Stopping a job that is not running is a conflict.
	if err := st.SetStatus("j1", agentjob.State{Status: agentjob.Done}); err != nil {
		t.Fatal(err)
	}
	if w3 := post("/api/jobs/j1/stop"); w3.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s", w3.Code, w3.Body.String())
	}

	// A job that is not in the store is a 404.
	if w4 := post("/api/jobs/nope/stop"); w4.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s", w4.Code, w4.Body.String())
	}
}
