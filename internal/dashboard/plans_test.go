package dashboard

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// planMultipart builds a multipart form body for POST /api/plans.
func planMultipart(t *testing.T, file, content, title, class, priority string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct{ key, value string }{
		{"title", title}, {"class", class}, {"priority", priority},
	} {
		if field.value == "" {
			continue
		}
		if err := w.WriteField(field.key, field.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// uploadPlan POSTs a plan file to /api/plans through the route table.
func uploadPlan(t *testing.T, s *Server, file, content, title, class, priority string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := planMultipart(t, file, content, title, class, priority)
	req := httptest.NewRequest("POST", "/api/plans", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func TestPlanUploadCreatesFileAndJob(t *testing.T) {
	s := newTestServer(t)

	w := uploadPlan(t, s, "my plan.md", "# do the thing\n- a\n- b\n", "", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp planUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v (%s)", err, w.Body.String())
	}
	if !planIDRe.MatchString(resp.ID) {
		t.Fatalf("plan id = %q", resp.ID)
	}
	if !strings.HasPrefix(resp.Title, "my plan") {
		t.Fatalf("title = %q", resp.Title)
	}
	if resp.JobStatus != agentjob.Ready {
		t.Fatalf("job status = %q, want ready", resp.JobStatus)
	}

	// The file is stored under <dataDir>/plans/<id>/ with the sanitized name.
	wantPath := filepath.Join(agentjob.DataDir(), "plans", resp.ID, "my-plan.md")
	if resp.Path != wantPath {
		t.Fatalf("path = %q, want %q", resp.Path, wantPath)
	}
	got, err := os.ReadFile(resp.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# do the thing\n- a\n- b\n" {
		t.Fatalf("file content = %q", got)
	}

	// The plan job carries the worker's recipe.
	st, err := s.openAgentJobStore()
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Get(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "Plan: my plan.md" {
		t.Fatalf("job title = %q", j.Title)
	}
	if j.Class != agentjob.ClassBuild || j.Priority != planDefaultPriority || j.Source != "plan" {
		t.Fatalf("class/priority/source = %q/%d/%q", j.Class, j.Priority, j.Source)
	}
	for _, want := range []string{resp.Path, "jarvis-jobs add -id " + resp.ID + "-1", "-depends " + resp.ID + "-1"} {
		if !strings.Contains(j.Instructions, want) {
			t.Errorf("instructions lack %q:\n%s", want, j.Instructions)
		}
	}
}

func TestPlanUploadOptions(t *testing.T) {
	s := newTestServer(t)

	w := uploadPlan(t, s, "plan.txt", "x", "My Plan", agentjob.ClassFix, "5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp planUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	st, _ := s.openAgentJobStore()
	j, err := st.Get(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "Plan: My Plan" {
		t.Fatalf("title = %q", j.Title)
	}
	if j.Class != agentjob.ClassFix || j.Priority != 5 {
		t.Fatalf("class/priority = %q/%d", j.Class, j.Priority)
	}
}

func TestPlanUploadRejections(t *testing.T) {
	s := newTestServer(t)

	// No file field at all.
	body, ct := planMultipart(t, "", "content", "", "", "")
	req := httptest.NewRequest("POST", "/api/plans", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no file: status = %d", w.Code)
	}

	// Bad class and bad priority.
	if w := uploadPlan(t, s, "p.md", "x", "", "bogus", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad class: status = %d", w.Code)
	}
	if w := uploadPlan(t, s, "p.md", "x", "", "", "-3"); w.Code != http.StatusBadRequest {
		t.Fatalf("negative priority: status = %d", w.Code)
	}

	// Oversized file: no job, no file left behind.
	old := maxPlanBytes
	maxPlanBytes = 10
	defer func() { maxPlanBytes = old }()
	if w := uploadPlan(t, s, "big.md", "0123456789ABCDEF", "", "", ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: status = %d, body %s", w.Code, w.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(agentjob.DataDir(), "plans"))
	if len(entries) != 0 {
		t.Fatalf("plans dir left %d entries after 413", len(entries))
	}
}

func TestPlanList(t *testing.T) {
	s := newTestServer(t)

	// Empty store: JSON is [], HTML is the empty state.
	req := httptest.NewRequest("GET", "/api/plans", nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "[]") {
		t.Fatalf("empty list: status = %d, body %s", w.Code, w.Body.String())
	}

	first := uploadPlan(t, s, "alpha plan.md", "one", "", "", "")
	second := uploadPlan(t, s, "beta plan.txt", "two", "Beta", "", "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("uploads: %d, %d", first.Code, second.Code)
	}
	type idOnly struct {
		ID string `json:"id"`
	}
	var firstResp, secondResp idOnly
	json.Unmarshal(first.Body.Bytes(), &firstResp)
	json.Unmarshal(second.Body.Bytes(), &secondResp)

	req = httptest.NewRequest("GET", "/api/plans", nil)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	var plans []planInfo
	if err := json.Unmarshal(w.Body.Bytes(), &plans); err != nil {
		t.Fatalf("list: %v (%s)", err, w.Body.String())
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	// Newest first.
	if plans[0].ID != secondResp.ID || plans[1].ID != firstResp.ID {
		t.Fatalf("order = [%s %s], want [%s %s]", plans[0].ID, plans[1].ID, secondResp.ID, firstResp.ID)
	}
	if plans[0].Title != "Beta" || plans[0].JobStatus != agentjob.Ready || plans[0].File != "beta-plan.txt" {
		t.Fatalf("first plan = %+v", plans[0])
	}
	if plans[1].File != "alpha-plan.md" || plans[1].UploadedAt == "" {
		t.Fatalf("second plan = %+v", plans[1])
	}

	// The HTML fragment renders both titles.
	req = httptest.NewRequest("GET", "/html/plans-section", nil)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("html: status = %d", w.Code)
	}
	for _, want := range []string{"Beta", "alpha-plan.md"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("html lacks %q:\n%s", want, w.Body.String())
		}
	}
}
