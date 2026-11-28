// Package api is loopd's HTTP layer (M4.1). This file (card ELR-104,
// M4.2d) adds NewSubmit: the POST /v1/tasks creation path with the
// submit checks — the task's worktree is created before the job is
// added to the store, and a branch another live task already uses is
// refused (the API answers 409).
package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/workspace"
)

// worktreeBase is the ref the task worktrees are cut from: the repo's
// current HEAD.
const worktreeBase = "HEAD"

// NewSubmit builds the Deps.Submit path with the submit checks (card
// ELR-104, M4.2d):
//
//   - it refuses the task when another job in the store that is not
//     done, failed or stopped (the store's parked status) has the same
//     Branch, and when ws.Create reports workspace.ErrBranchInUse (the
//     branch is checked out in a worktree the store does not know) —
//     both as an error wrapping ErrConflict, so the API answers 409;
//   - it generates the branch loop/<id> when the request names none;
//   - it Create()s the worktree, sets Spec.Workspace to the worktree's
//     path, adds the job to the store and calls loopSubmit(id) — a nil
//     loopSubmit (a server without a loop) leaves the job ready in the
//     store.
//
// Any error after the worktree exists removes it again before the error
// is returned.
func NewSubmit(store *agentjob.Store, ws *workspace.Manager, loopSubmit func(jobID string) error) func(TaskRequest) (string, error) {
	return func(req TaskRequest) (string, error) {
		id, err := newTaskID()
		if err != nil {
			return "", err
		}
		branch := req.Branch
		if branch == "" {
			branch = "loop/" + id
		}
		if other, err := liveBranchJob(store, branch); err != nil {
			return "", err
		} else if other != "" {
			return "", fmt.Errorf("%w: branch %s is used by live job %s", ErrConflict, branch, other)
		}
		ctx := context.Background()
		path, err := ws.Create(ctx, id, branch, worktreeBase)
		if err != nil {
			if errors.Is(err, workspace.ErrBranchInUse) {
				return "", fmt.Errorf("%w: branch %s is checked out in another worktree", ErrConflict, branch)
			}
			return "", err
		}
		// Rollback removes the worktree this submit created; a failed
		// remove is reported alongside the original error.
		rollback := func(err error) error {
			if rerr := ws.Remove(ctx, id); rerr != nil {
				err = fmt.Errorf("%v (worktree rollback: %v)", err, rerr)
			}
			return err
		}
		if _, err := store.Ingest(agentjob.Spec{
			ID:           id,
			Title:        req.Title,
			Instructions: req.Instructions,
			Branch:       branch,
			Workspace:    path,
			Acceptance:   req.Acceptance,
			DependsOn:    req.DependsOn,
			Driver:       "loop",
			Class:        agentjob.ClassBuild,
			Source:       "api",
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			return "", rollback(err)
		}
		if loopSubmit != nil {
			if err := loopSubmit(id); err != nil {
				return "", rollback(err)
			}
		}
		return id, nil
	}
}

// liveBranchJob returns the id of a job that is not finished or stopped
// and already uses branch, or "" when no live job holds it: done,
// failed, skipped and parked (the plan's "stopped") free their branch,
// every other status (ready, claimed, running, paused, waiting) holds
// it. An empty branch never conflicts.
func liveBranchJob(store *agentjob.Store, branch string) (string, error) {
	if branch == "" {
		return "", nil
	}
	jobs, err := store.List()
	if err != nil {
		return "", fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Branch != branch {
			continue
		}
		switch j.Status {
		case agentjob.Done, agentjob.Failed, agentjob.Skipped, agentjob.Parked:
		default:
			return j.ID, nil
		}
	}
	return "", nil
}
