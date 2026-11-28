// Package workspace gives each runtime task its own git worktree on a
// mounted volume (card ELR-103): two tasks never share one.
//
// The git calls are copied from internal/taskrunner's setupTaskWorktree
// (a private method, not importable): same repo, same worktree
// discipline.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBranchInUse is returned by Create when the branch is already
// checked out in another worktree.
var ErrBranchInUse = errors.New("workspace: branch already checked out")

// Manager creates and removes one worktree per job under Root, cut from
// Repo.
type Manager struct {
	Repo string // the git repository the worktrees are cut from
	Root string // directory holding the worktrees (a mounted volume)
}

// Path returns the worktree directory for jobID (Root/jobID).
func (m *Manager) Path(jobID string) string {
	return filepath.Join(m.Root, jobID)
}

// Create makes the worktree for jobID:
//
//	git worktree add -b <branch> <Root>/<jobID> <base>
//
// A jobID that is empty or contains "/" or ".." is refused. A branch
// already checked out in another worktree is refused with
// ErrBranchInUse. A second Create for the same jobID and branch
// returns the existing path.
func (m *Manager) Create(ctx context.Context, jobID, branch, base string) (string, error) {
	if err := checkJobID(jobID); err != nil {
		return "", err
	}
	path := m.Path(jobID)
	branches, err := m.worktreeBranches(ctx)
	if err != nil {
		return "", err
	}
	if at, ok := branches[path]; ok {
		if at != branch {
			return "", fmt.Errorf("workspace: job %s already has a worktree on branch %s", jobID, at)
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil // a second Create for the same job
		}
		// The registration is dangling (the directory is gone):
		// prune it and re-read the list before the branch check.
		if _, err := gitRun(ctx, m.Repo, "worktree", "prune"); err != nil {
			return "", err
		}
		branches, err = m.worktreeBranches(ctx)
		if err != nil {
			return "", err
		}
	}
	if _, ok := branchOwner(branches, branch); ok {
		return "", ErrBranchInUse
	}
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		// A crashed run can leave the directory without its
		// registration: drop it before adding, like the taskrunner.
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("workspace: remove stale %s: %v", path, err)
		}
		gitRun(ctx, m.Repo, "worktree", "prune")
	}
	if out, err := gitRun(ctx, m.Repo, "worktree", "add", "-b", branch, path, base); err != nil {
		return "", fmt.Errorf("workspace: create %s from %s: %v (%s)", path, base, err, out)
	}
	return path, nil
}

// Remove drops the worktree for jobID: git worktree remove <Root>/<jobID>.
func (m *Manager) Remove(ctx context.Context, jobID string) error {
	if err := checkJobID(jobID); err != nil {
		return err
	}
	path := m.Path(jobID)
	if _, err := gitRun(ctx, m.Repo, "worktree", "remove", path); err != nil {
		// A crashed run can leave the registration without its
		// directory: prune, then try once more.
		gitRun(ctx, m.Repo, "worktree", "prune")
		if out, err := gitRun(ctx, m.Repo, "worktree", "remove", path); err != nil {
			return fmt.Errorf("workspace: remove %s: %v (%s)", path, err, out)
		}
	}
	return nil
}

// checkJobID refuses jobIDs that could escape Root: empty, or with "/"
// or "..".
func checkJobID(jobID string) error {
	if jobID == "" || strings.Contains(jobID, "/") || strings.Contains(jobID, "..") {
		return fmt.Errorf("workspace: bad jobID %q", jobID)
	}
	return nil
}

// worktreeBranches runs "git worktree list --porcelain" in the repo and
// parses it into a path -> branch map.
func (m *Manager) worktreeBranches(ctx context.Context) (map[string]string, error) {
	list, err := gitRun(ctx, m.Repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeBranches(list), nil
}

// parseWorktreeBranches parses "git worktree list --porcelain" into a
// path -> branch map ("" for a detached HEAD).
func parseWorktreeBranches(porcelain string) map[string]string {
	branches := map[string]string{}
	var path string
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
			branches[path] = ""
		case strings.HasPrefix(line, "branch "), path != "":
			branches[path] = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	return branches
}

// branchOwner reports the worktree path that has branch checked out.
func branchOwner(branches map[string]string, branch string) (string, bool) {
	for p, b := range branches {
		if b == branch {
			return p, true
		}
	}
	return "", false
}

// gitRun runs git in dir and returns its trimmed output (copied from
// internal/taskrunner).
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
