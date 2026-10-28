package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/slots"
)

func TestEngineLogs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_STATE_DIR", dir)
	os.WriteFile(filepath.Join(dir, "agent.log"), []byte("line1\nline2\nline3\n"), 0o600)
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) }))
	defer eng.Close()
	s := &Server{engineClient: &slots.Client{BaseURL: eng.URL, HTTP: eng.Client()}}

	rec := httptest.NewRecorder()
	s.handleEngineLogs(rec, httptest.NewRequest("GET", "/api/engine/logs?tail=10", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"healthy":true`) || !strings.Contains(body, "line3") || strings.Contains(body, "line1") {
		t.Fatalf("unexpected body: %s", body)
	}

	rec = httptest.NewRecorder()
	s.handleEngineLogs(rec, httptest.NewRequest("GET", "/api/engine/logs?file=../etc/passwd", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal not rejected: %d", rec.Code)
	}
}
