package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindow(t *testing.T) {
	all := []int{1, 2, 3, 4, 5}
	page, info := window(all, 0, 2)
	if len(page) != 2 || !info.HasMore || info.Total != 5 || info.Hidden != 3 {
		t.Fatalf("first %+v %+v", page, info)
	}
	page, info = window(all, 2, 2)
	if len(page) != 2 || info.Hidden != 1 {
		t.Fatalf("mid %+v %+v", page, info)
	}
	page, info = window(all, 4, 2)
	if len(page) != 1 || info.HasMore {
		t.Fatalf("last %+v %+v", page, info)
	}
}

func TestApprovalsArePaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		rec := JobRecord{ID: fmt.Sprintf("deny-%d", i), Status: "denied", StartedAt: "2026-10-23T00:00:00Z", Type: "voice"}
		b, _ := json.Marshal(rec)
		f.Write(append(b, '\n'))
		if i < 10 {
			f.Write(append(b, '\n'))
		}
	}
	f.Close()

	s := &Server{historyPath: path}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /html/approvals", s.handleApprovals)
	mux.HandleFunc("GET /api/approvals", s.handleApprovals)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/approvals", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	body := rec.Body.String()
	if n := strings.Count(body, `class="approval-card"`); n != listPageSize {
		t.Fatalf("first paint cards=%d body=%s", n, body[:min(400, len(body))])
	}
	if !strings.Contains(body, "denied in history") || !strings.Contains(body, "older stay in jobs.jsonl") {
		t.Fatalf("missing archive copy: %s", body[:min(500, len(body))])
	}
	if !strings.Contains(body, `href="/jobs/deny-`) {
		t.Fatal("approval card lost Log link")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/html/approvals?offset=25", nil))
	if n := strings.Count(rec.Body.String(), `class="approval-card"`); n != listPageSize {
		t.Fatalf("page 2 cards=%d", n)
	}
}

