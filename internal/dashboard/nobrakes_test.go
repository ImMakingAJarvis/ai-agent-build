package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// nobrakesTestServer builds a Server whose data dir is a fresh temp dir,
// with the given audit entries pre-written to the NoBrakes audit log.
func nobrakesTestServer(t *testing.T, entries ...nobrakes.AuditEntry) *Server {
	t.Helper()
	dir := t.TempDir()
	if len(entries) > 0 {
		l, err := nobrakes.NewAuditLog(filepath.Join(dir, "nobrakes", "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if err := l.Record(e); err != nil {
				t.Fatal(err)
			}
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{historyPath: filepath.Join(dir, "jobs.jsonl")}
}

func getAudit(t *testing.T, s *Server) nobrakesAuditResp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nobrakes/audit", nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/nobrakes/audit: %d, body %s", w.Code, w.Body.String())
	}
	var resp nobrakesAuditResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, w.Body.String())
	}
	return resp
}

// TestNobrakesAuditEndpointEmpty: no log file yet is an empty list, not an
// error — NoBrakes has simply not auto-run anything.
func TestNobrakesAuditEndpointEmpty(t *testing.T) {
	resp := getAudit(t, nobrakesTestServer(t))
	if resp.Entries == nil || len(resp.Entries) != 0 {
		t.Fatalf("entries = %#v, want empty non-nil", resp.Entries)
	}
}

// TestNobrakesAuditEndpointListsEntries: the endpoint returns every audit
// entry, oldest first, and names the log file it read.
func TestNobrakesAuditEndpointListsEntries(t *testing.T) {
	e1 := nobrakes.AuditEntry{Timestamp: "2026-11-03T03:00:00Z", Window: "2026-11-03", JobID: "n1", Title: "low", Trigger: nobrakes.TriggerAutoMode}
	e2 := nobrakes.AuditEntry{Timestamp: "2026-11-04T03:00:00Z", Window: "2026-11-04", JobID: "n2", Title: "high", Trigger: nobrakes.TriggerAutoMode}
	resp := getAudit(t, nobrakesTestServer(t, e1, e2))
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", resp.Entries)
	}
	if resp.Entries[0] != e1 || resp.Entries[1] != e2 {
		t.Fatalf("entries out of order or mangled: %+v", resp.Entries)
	}
	if filepath.Base(filepath.Dir(resp.Path)) != "nobrakes" || filepath.Base(resp.Path) != "audit.jsonl" {
		t.Fatalf("path = %q, want .../nobrakes/audit.jsonl", resp.Path)
	}
}
