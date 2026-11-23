package taskrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// PRClient opens and updates the epic's pull request (C3,
// docs/work-board/PLAN.md: "When every card is Done, open one PR from the
// epic branch to main"). The runner opens it as a draft as soon as the
// first task is merged and pushed, so every finished part is visible and
// reviewable on GitHub while the rest of the epic runs; each later task
// is one more pushed commit on the same PR. When the last task is Done the
// draft is marked ready for review.
type PRClient interface {
	// Ensure returns the open PR for head, creating it as a draft against
	// base when there is none. created reports whether this call made it.
	Ensure(ctx context.Context, head, base, title, body string) (url string, created bool, err error)
	// Ready marks head's open PR ready for review (a no-op when it already
	// is).
	Ready(ctx context.Context, head string) error
}

// GHPRClient is the production PRClient: the gh CLI, authenticated with
// the GitHub App installation token (ghapp-token, docs/github-access.md),
// run in the repo so gh resolves the GitHub repository from origin.
type GHPRClient struct {
	// Repo is the git checkout gh runs in.
	Repo string
	// Token returns a GitHub token; nil runs `ghapp-token`.
	Token func() (string, error)
}

func (g GHPRClient) gh(ctx context.Context, args ...string) (string, error) {
	tok, err := g.token()
	if err != nil {
		return "", fmt.Errorf("github token: %w", err)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = g.Repo
	cmd.Env = append(os.Environ(), "GH_TOKEN="+tok, "GH_PROMPT_DISABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh %s: %v (%s)", args[0]+" "+args[1], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (g GHPRClient) token() (string, error) {
	if g.Token != nil {
		return g.Token()
	}
	out, err := exec.Command("ghapp-token").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Ensure implements PRClient.
func (g GHPRClient) Ensure(ctx context.Context, head, base, title, body string) (string, bool, error) {
	url, err := g.gh(ctx, "pr", "list", "--head", head, "--state", "open", "--json", "url", "--jq", ".[0].url // empty")
	if err != nil {
		return "", false, err
	}
	if url != "" {
		return url, false, nil
	}
	out, err := g.gh(ctx, "pr", "create", "--draft", "--base", base, "--head", head, "--title", title, "--body", body)
	if err != nil {
		return "", false, err
	}
	// gh pr create prints the URL as its last line.
	lines := strings.Split(out, "\n")
	return strings.TrimSpace(lines[len(lines)-1]), true, nil
}

// Ready implements PRClient.
func (g GHPRClient) Ready(ctx context.Context, head string) error {
	draft, err := g.gh(ctx, "pr", "view", head, "--json", "isDraft", "--jq", ".isDraft")
	if err != nil {
		return err
	}
	if draft != "true" {
		return nil
	}
	_, err = g.gh(ctx, "pr", "ready", head)
	return err
}

// publishEpic runs after a task's merge has been pushed: it makes sure the
// epic has its (draft) PR, links it on the epic once, and marks it ready
// for review when every task of the epic is Done. PR trouble never fails
// the task — the work is merged and pushed — it is reported on the epic
// instead, so a missing PR is visible on the board, not only in the log.
func (r *Runner) publishEpic(ctx context.Context, projectID string, epic *plane.WorkItem, epicCode, epicBranch, doneID, justDone string) {
	if r.PRs == nil {
		return
	}
	title := fmt.Sprintf("%s: %s", epicCode, epic.Name)
	body := fmt.Sprintf("Epic %s, built task by task by the task runner.\n\n"+
		"Each finished task is merged into `%s` and pushed here, so this PR grows one commit per task. "+
		"It stays a draft until every task of the epic is Done, then it is marked ready for review.\n\n"+
		"Per-task log: `%s` on this branch.", epicCode, epicBranch, r.NotesFile)
	url, created, err := r.PRs.Ensure(ctx, epicBranch, "main", title, body)
	if err != nil {
		r.logf("epic PR for %s: %v", epicBranch, err)
		r.commentOnce(ctx, projectID, epic.ID, "taskrunner: could not open the epic PR",
			fmt.Sprintf("taskrunner: could not open the epic PR for %s (the work is pushed to that branch): %v", epicBranch, err))
		return
	}
	if created {
		r.comment(ctx, projectID, epic.ID, fmt.Sprintf("taskrunner: draft PR for %s: %s — each finished task is pushed to it", epicBranch, url))
	}
	if !r.allTasksDone(ctx, projectID, epic.ID, doneID, justDone) {
		return
	}
	if err := r.PRs.Ready(ctx, epicBranch); err != nil {
		r.logf("mark epic PR ready: %v", err)
		r.comment(ctx, projectID, epic.ID, fmt.Sprintf("taskrunner: every task is Done, but marking %s ready failed: %v", url, err))
		return
	}
	r.commentOnce(ctx, projectID, epic.ID, "ready for review: "+url,
		fmt.Sprintf("taskrunner: every task is Done — PR ready for review: %s", url))
}

// allTasksDone reports whether every sub-item of the epic is Done. justDone
// counts as Done even if its state move has not landed yet.
func (r *Runner) allTasksDone(ctx context.Context, projectID, epicID, doneID, justDone string) bool {
	items, err := r.Board.ListWorkItems(ctx, projectID)
	if err != nil {
		r.logf("list work items: %v", err)
		return false
	}
	n := 0
	for _, it := range items {
		if it.Parent != epicID {
			continue
		}
		n++
		if it.ID != justDone && it.State != doneID {
			return false
		}
	}
	return n > 0
}

// commentOnce posts text unless a comment on the item already contains
// key, so a repeated failure or a re-run does not stack the same note.
func (r *Runner) commentOnce(ctx context.Context, projectID, itemID, key, text string) {
	if r.hasComment(ctx, projectID, itemID, key) {
		return
	}
	r.comment(ctx, projectID, itemID, text)
}
