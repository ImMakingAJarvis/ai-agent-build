package taskrunner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// fakePRs records the PR calls the runner makes.
type fakePRs struct {
	mu      sync.Mutex
	open    map[string]string // head -> url
	ensures []string          // heads Ensure was called for
	readies []string          // heads Ready was called for
	titles  []string
}

func (f *fakePRs) Ensure(ctx context.Context, head, base, title, body string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, head)
	if f.open == nil {
		f.open = map[string]string{}
	}
	if url, ok := f.open[head]; ok {
		return url, false, nil
	}
	f.titles = append(f.titles, title)
	f.open[head] = "https://github.test/pr/1"
	return f.open[head], true, nil
}

func (f *fakePRs) Ready(ctx context.Context, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readies = append(f.readies, head)
	return nil
}

// withOrigin gives the runner's repo an origin remote (a local bare repo)
// so pushEpic really pushes.
func withOrigin(t *testing.T, repo, url string) {
	t.Helper()
	for _, args := range [][]string{{"remote", "add", "origin", url}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
}

func bareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}
	return dir
}

func epicComments(board *fakeBoard, id string) string {
	cs, _ := board.ListComments(context.Background(), "proj1", id)
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.CommentHTML + "\n")
	}
	return b.String()
}

// TestRunOpensDraftPRThenReady: the first finished task pushes the epic
// branch and opens a draft PR (linked on the epic); the last finished task
// marks it ready for review.
func TestRunOpensDraftPRThenReady(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2, Labels: []string{"epic"}})
	board.add(plane.WorkItem{ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b")})
	board.add(plane.WorkItem{ID: "card2", Name: "health docs", State: "s-ready", Parent: "epic1", SequenceID: 4,
		DescriptionHTML: cardHTML("docs", "document /health", "HEALTH.md", "", "test -f HEALTH.md", "S", "local-27b")})
	prs := &fakePRs{}
	r := testRunner(t, board, &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "health.py"},
		contentChecker{file: "health.py", want: "ok"})
	r.PRs = prs
	origin := bareRepo(t)
	withOrigin(t, r.Repo, origin)

	if res, err := runJob(t, r, "card1", "epic1", "model:local-27b"); err != nil || !res.Passed {
		t.Fatalf("Run card1 = %+v, %v", res, err)
	}
	if out, err := exec.Command("git", "--git-dir", origin, "log", "--oneline", "epics/TST-2").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), "task-3") {
		t.Fatalf("epic branch not pushed after the first task: %v (%s)", err, out)
	}
	if len(prs.ensures) != 1 || prs.ensures[0] != "epics/TST-2" || len(prs.readies) != 0 {
		t.Fatalf("after card1: ensures=%v readies=%v, want one Ensure and no Ready", prs.ensures, prs.readies)
	}
	if prs.titles[0] != "TST-2: Add a /health endpoint" {
		t.Errorf("PR title = %q", prs.titles[0])
	}
	if c := epicComments(board, "epic1"); !strings.Contains(c, "draft PR for epics/TST-2: https://github.test/pr/1") {
		t.Errorf("epic missing the draft PR link:\n%s", c)
	}

	r.Harness = &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "HEALTH.md"}
	r.Checks = contentChecker{file: "HEALTH.md", want: "ok"}
	if res, err := runJob(t, r, "card2", "epic1", "model:local-27b"); err != nil || !res.Passed {
		t.Fatalf("Run card2 = %+v, %v", res, err)
	}
	if len(prs.ensures) != 2 || len(prs.readies) != 1 {
		t.Fatalf("after card2: ensures=%v readies=%v, want the PR reused and marked ready", prs.ensures, prs.readies)
	}
	c := epicComments(board, "epic1")
	if strings.Count(c, "draft PR for") != 1 {
		t.Errorf("draft PR link posted more than once:\n%s", c)
	}
	if !strings.Contains(c, "PR ready for review: https://github.test/pr/1") {
		t.Errorf("epic missing the ready comment:\n%s", c)
	}
}

// TestRunPushFailureIsReported: a push that fails is said on the card and
// no PR is attempted (it would not carry the work).
func TestRunPushFailureIsReported(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2, Labels: []string{"epic"}})
	board.add(plane.WorkItem{ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b")})
	prs := &fakePRs{}
	r := testRunner(t, board, &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "health.py"},
		contentChecker{file: "health.py", want: "ok"})
	r.PRs = prs
	withOrigin(t, r.Repo, filepath.Join(t.TempDir(), "missing.git"))

	if res, err := runJob(t, r, "card1", "epic1", "model:local-27b"); err != nil || !res.Passed {
		t.Fatalf("Run = %+v, %v (a failed push must not fail a merged task)", res, err)
	}
	if c := epicComments(board, "card1"); !strings.Contains(c, "push of epics/TST-2 failed") {
		t.Errorf("card missing the push failure:\n%s", c)
	}
	if len(prs.ensures) != 0 {
		t.Errorf("Ensure called %d times after a failed push, want 0", len(prs.ensures))
	}
}
