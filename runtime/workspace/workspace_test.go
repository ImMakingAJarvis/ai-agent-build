package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a real git repo with one commit in a temp dir.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	must(t, repo, "init")
	must(t, repo, "config", "user.email", "test@example.com")
	must(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, repo, "add", "a.txt")
	must(t, repo, "commit", "-m", "initial")
	return repo
}

func must(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateMakesWorktreeOnBranch(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	path, err := m.Create(context.Background(), "job1", "feature-x", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := m.Path("job1"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
	if got := must(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature-x" {
		t.Fatalf("branch = %s, want feature-x", got)
	}
}

func TestTwoJobsGetDistinctPaths(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	p1, err := m.Create(context.Background(), "job1", "b1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.Create(context.Background(), "job2", "b2", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("two jobs share a path: %s", p1)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatal(err)
	}
}

func TestSameBranchDifferentJobRefused(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	if _, err := m.Create(context.Background(), "job1", "shared", "HEAD"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Create(context.Background(), "job2", "shared", "HEAD")
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
}

func TestCreateSameJobTwiceReturnsSamePath(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	p1, err := m.Create(context.Background(), "job1", "b1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.Create(context.Background(), "job1", "b1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("first = %s, second = %s", p1, p2)
	}
}

func TestRemoveDeletesWorktree(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	path, err := m.Create(context.Background(), "job1", "b1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(context.Background(), "job1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := must(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, path) {
		t.Fatalf("worktree still registered:\n%s", out)
	}
}

func TestBadJobIDRejected(t *testing.T) {
	repo := newRepo(t)
	m := Manager{Repo: repo, Root: t.TempDir()}
	for _, id := range []string{"../x", "a/b", ""} {
		if _, err := m.Create(context.Background(), id, "b", "HEAD"); err == nil {
			t.Errorf("Create(%q) succeeded, want error", id)
		}
	}
}
