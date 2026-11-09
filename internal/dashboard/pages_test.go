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
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
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

	tmpl, err := template.New("").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{historyPath: path, tmpl: tmpl}
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

// TestWaitStatusText: a waiting row shows what it waits on plus its age;
// other statuses pass through unchanged.
func TestWaitStatusText(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	mk := func(status string, since string, waitOn ...string) agentjob.Job {
		j := agentjob.Job{}
		j.Status = status
		j.WaitingSince = since
		j.WaitOn = waitOn
		return j
	}
	cases := []struct {
		name string
		job  agentjob.Job
		want string
	}{
		{"two corrs", mk(agentjob.Waiting, "2026-11-08T11:48:00Z", "J-img-1", "J-img-2"), "waiting on J-img-1, J-img-2 (12m)"},
		{"no corrs", mk(agentjob.Waiting, "2026-11-08T11:59:15Z"), "waiting (45s)"},
		{"hours", mk(agentjob.Waiting, "2026-11-08T09:57:00Z", "J-img-1"), "waiting on J-img-1 (2h3m)"},
		{"days", mk(agentjob.Waiting, "2026-11-06T10:00:00Z", "J-img-1"), "waiting on J-img-1 (2d2h)"},
		{"missing since", mk(agentjob.Waiting, "", "J-img-1"), "waiting on J-img-1 (?)"},
		{"running passes through", mk("running", "", "J-img-1"), "running"},
		{"ready passes through", mk("ready", ""), "ready"},
	}
	for _, c := range cases {
		if got := WaitStatusText(c.job, now); got != c.want {
			t.Errorf("%s: WaitStatusText = %q, want %q", c.name, got, c.want)
		}
	}
}

