package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/workspace"
)

// submitHarness is the NewSubmit bench (card ELR-104): a real git repo,
// a real store, a real worktree manager over a temp root, and a Server
// wired with NewSubmit — so the tests go through POST /v1/tasks.
type submitHarness struct {
	srv       *Server
	store     *agentjob.Store
	ws        *workspace.Manager
	repo      string
	root      string
	submu     sync.Mutex
	submitted []string
}

func newSubmitHarness(t *testing.T) *submitHarness {
	t.Helper()
	repo := newRepo(t)
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	root := t.TempDir()
	ws := &workspace.Manager{Repo: repo, Root: root}
	h := &submitHarness{store: store, ws: ws, repo: repo, root: root}
	h.srv = New(Deps{
		Store:    store,
		Redactor: redact.New(nil),
		Token:    testToken,
		Version:  "test",
		Submit: NewSubmit(store, ws, func(jobID string) error {
			h.submu.Lock()
			h.submitted = append(h.submitted, jobID)
			h.submu.Unlock()
			return nil
		}),
	})
	return h
}

// postTask POSTs body to /v1/tasks on the harness server.
func postTask(t *testing.T, h *submitHarness, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithToken(t, h.srv.Handler(), "/v1/tasks", body)
}

// taskIDFrom201 pulls the created id out of a 201 response, failing the
// test when the code is anything else.
func taskIDFrom201(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.ID
}

func TestSubmitSetsWorkspace(t *testing.T) {
	h := newSubmitHarness(t)
	id := taskIDFrom201(t, postTask(t, h, `{"title":"t","instructions":"do it","branch":"b1"}`))
	job, err := h.store.Get(id)
	if err != nil {
		t.Fatalf("job not in the store: %v", err)
	}
	if job.Workspace != h.ws.Path(id) {
		t.Fatalf("job.Workspace = %q, want %q", job.Workspace, h.ws.Path(id))
	}
	if _, err := os.Stat(job.Workspace); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
	h.submu.Lock()
	submitted := append([]string(nil), h.submitted...)
	h.submu.Unlock()
	if len(submitted) != 1 || submitted[0] != id {
		t.Fatalf("submitted = %v, want [%s]", submitted, id)
	}
}

func TestSubmitTwoTasksTwoWorktrees(t *testing.T) {
	h := newSubmitHarness(t)
	ids := []string{}
	for i, b := range []string{"b1", "b2"} {
		ids = append(ids, taskIDFrom201(t,
			postTask(t, h, fmt.Sprintf(`{"instructions":"t%d","branch":"%s"}`, i+1, b))))
	}
	j1, err := h.store.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	j2, err := h.store.Get(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if j1.Workspace == j2.Workspace {
		t.Fatalf("two tasks share a worktree: %s", j1.Workspace)
	}
	for _, p := range []string{j1.Workspace, j2.Workspace} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("worktree missing: %v", err)
		}
	}
}

func TestSubmitSameBranchRefused(t *testing.T) {
	h := newSubmitHarness(t)
	if rec := postTask(t, h, `{"instructions":"first","branch":"shared"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first: code = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	rec := postTask(t, h, `{"instructions":"second","branch":"shared"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second: code = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	jobs, err := h.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1 — the refused task must not land in the store", len(jobs))
	}
}

func TestSubmitFinishedTaskBranchReusable(t *testing.T) {
	h := newSubmitHarness(t)
	id := taskIDFrom201(t, postTask(t, h, `{"instructions":"first","branch":"shared"}`))
	if err := h.store.SetStatus(id, agentjob.State{Status: agentjob.Done}); err != nil {
		t.Fatal(err)
	}
	// The runtime's job lifecycle removes a finished job's worktree and
	// deletes its branch; the test stands in for both.
	if err := h.ws.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", h.repo, "branch", "-D", "shared").CombinedOutput(); err != nil {
		t.Fatalf("git branch -D shared: %v\n%s", err, out)
	}
	if rec := postTask(t, h, `{"instructions":"second","branch":"shared"}`); rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSubmitEmptyBranchGetsLoopPrefix(t *testing.T) {
	h := newSubmitHarness(t)
	id := taskIDFrom201(t, postTask(t, h, `{"instructions":"no branch"}`))
	job, err := h.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "loop/" + id; job.Branch != want {
		t.Fatalf("job.Branch = %q, want %q", job.Branch, want)
	}
}

func TestSubmitRollsBackWorktreeOnAddError(t *testing.T) {
	h := newSubmitHarness(t)
	// Fail the store add: the jobs dir refuses the new job's directory.
	jobsDir := filepath.Join(h.store.Dir(), "jobs")
	if err := os.Chmod(jobsDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(jobsDir, 0o755)
	rec := postTask(t, h, `{"instructions":"x","branch":"b1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	// The worktree the submit created is gone again.
	entries, err := os.ReadDir(h.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("worktree root not empty after the failed add: %v", entries)
	}
}

// newRepo makes a real git repo with one commit in a temp dir (the same
// bench the workspace package tests with).
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitMust(t, repo, "init")
	gitMust(t, repo, "config", "user.email", "test@example.com")
	gitMust(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitMust(t, repo, "add", "a.txt")
	gitMust(t, repo, "commit", "-m", "initial")
	return repo
}

func gitMust(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
