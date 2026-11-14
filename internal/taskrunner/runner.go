package taskrunner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// The card states the runner moves (the project's standard state names).
const (
	StateInProgress = "In progress"
	StateDone       = "Done"
	StateBlocked    = "Blocked"
)

// DefaultNotesFile is appended with one line per Done card (PLAN.md,
// execution step 7).
const DefaultNotesFile = "docs/work-board/NOTES.md"

// Result is the outcome of one Run. The cmd/task-runner handler maps it
// onto the job store: Waiting → skipped (the bridge re-triggers the job
// while the card is still Ready), Blocked → done (settled: a human moves
// the card), Passed → done, anything else → failed.
type Result struct {
	// Waiting: dependencies not all Done (or cloud, not wired yet). The
	// card was left where it was.
	Waiting bool
	// Blocked: the card moved to Blocked (no usable model, placement
	// blocked, or three failed attempts).
	Blocked bool
	// Passed: the card moved to Done.
	Passed bool
	// Summary is the one-line outcome for the store (and NOTES.md).
	Summary string
	// Error carries the last check output on failure (store error field).
	Error string
}

// BoardAPI is the slice of the Plane client the execution loop uses.
// *plane.Client satisfies it; tests substitute an in-memory board.
type BoardAPI interface {
	ListStates(ctx context.Context, projectID string) ([]plane.State, error)
	ListWorkItems(ctx context.Context, projectID string) ([]plane.WorkItem, error)
	GetWorkItem(ctx context.Context, projectID, itemID string) (*plane.WorkItem, error)
	SetState(ctx context.Context, projectID, itemID, stateID string) (*plane.WorkItem, error)
	AddComment(ctx context.Context, projectID, itemID, html string) (*plane.Comment, error)
	ListComments(ctx context.Context, projectID, itemID string) ([]plane.Comment, error)
	ListPages(ctx context.Context, projectID string) ([]plane.Page, error)
	GetPage(ctx context.Context, projectID, pageID string) (*plane.Page, error)
}

// Runner runs run_task jobs: read the card, wait for dependencies,
// decide placement, drive the harness against the acceptance check for
// up to MaxAttempts attempts.
type Runner struct {
	// Board is the Plane client (cards, states, comments, plan page).
	Board BoardAPI
	// Registry is the loaded models file (executor lookup).
	Registry *registry.File
	// Repo is the git repository the epic/task branches live in.
	Repo string
	// NotesFile is appended with one line per Done card, relative to
	// Repo. Empty means DefaultNotesFile.
	NotesFile string
	// Harness edits files (Aider in production, a fake in tests).
	Harness Harness
	// Checks runs the acceptance check plus build/lint.
	Checks Checker
	// Live returns the current placement state (free memory, present
	// locations). fit.Live plus placement.Detect in production.
	Live func(ctx context.Context) (placement.State, error)
	// MaxAttempts harness+check rounds before the card is Blocked.
	// Zero means 3.
	MaxAttempts int
	// Caps submits a card's capability and waits for its Result — the
	// no-LLM-round path for a card that carries a capability: (plan §3.7).
	// Nil means no capability client is wired; a capability card is then
	// Blocked (a normal card is unaffected). Production wraps the broker
	// connection with NewQueueCapabilityClient; tests use a scripted one.
	Caps CapabilityClient
	// CapabilityTimeout bounds the wait for a capability's Result. Zero
	// means DefaultCapabilityTimeout.
	CapabilityTimeout time.Duration
	// Log receives progress; nil is fine.
	Log *slog.Logger
}

// DefaultCapabilityTimeout bounds how long a card waits for its
// capability's Result before the runner settles it as a timeout and
// Blocks the card. Capabilities are bounded (the catalog carries each one's
// typical runtime); the bound is a safety net for a worker that dies
// without publishing a Result.
const DefaultCapabilityTimeout = 30 * time.Minute

// Run executes one run_task job. A non-nil error is a runner/
// infrastructure failure (no card state was relied on; the store entry
// should be failed and the job retried next round). A nil error means
// the card was settled — see Result.
func (r *Runner) Run(ctx context.Context, job *Job) (Result, error) {
	r.defaults()
	projectID := job.Board.ProjectID

	card, err := r.Board.GetWorkItem(ctx, projectID, job.Item.ID)
	if err != nil {
		return Result{}, fmt.Errorf("fetch card: %w", err)
	}
	task, err := planworker.ParseTaskCard(card.DescriptionHTML)
	if err != nil {
		r.blockCard(ctx, projectID, card.ID, "not a task card: "+err.Error())
		return Result{Blocked: true, Summary: "not a task card"}, nil
	}
	epic, err := r.Board.GetWorkItem(ctx, projectID, job.Item.Parent)
	if err != nil {
		return Result{}, fmt.Errorf("fetch epic: %w", err)
	}

	states, err := r.Board.ListStates(ctx, projectID)
	if err != nil {
		return Result{}, fmt.Errorf("list states: %w", err)
	}
	// State names are matched case-insensitively, like the bridge: the
	// live projects migrated from AAB carry Plane's default "In
	// Progress", while plane-epic scaffolds "In progress".
	stateByID := map[string]string{}
	doneID, inProgressID := "", ""
	for _, s := range states {
		stateByID[s.ID] = s.Name
		switch {
		case strings.EqualFold(s.Name, StateDone):
			doneID = s.ID
		case strings.EqualFold(s.Name, StateInProgress):
			inProgressID = s.ID
		}
	}
	if inProgressID == "" || doneID == "" {
		return Result{}, fmt.Errorf("project %s has no %q or %q state", projectID, StateInProgress, StateDone)
	}

	// Dependencies must all be Done before anything runs. The dep ids
	// name sibling cards (their plan-task: markers), not items.
	items, err := r.Board.ListWorkItems(ctx, projectID)
	if err != nil {
		return Result{}, fmt.Errorf("list work items: %w", err)
	}
	if missing := r.missingDeps(epic, task.Dependencies, items, doneID, stateByID); len(missing) > 0 {
		ids := make([]string, len(missing))
		for i, d := range missing {
			ids[i] = d.ID
		}
		// The needle is the dep ids only: the details (a dep's state
		// name) change while it is in flight, and the comment must not
		// be re-posted for that.
		needle := "waiting on: " + strings.Join(ids, ",")
		if !r.hasComment(ctx, projectID, card.ID, needle) {
			r.comment(ctx, projectID, card.ID, "taskrunner: waiting on: "+strings.Join(ids, ",")+" — "+
				strings.Join(details(missing), ", ")+" (the card stays in Ready)")
		}
		return Result{Waiting: true, Summary: "waiting on " + strings.Join(ids, ", ")}, nil
	}

	// A capability card (plan §3.7): the executor is a capability worker,
	// not an LLM round. No placement, no harness — submit to cap.<cap>
	// and wait for the Result. Placed before the placement block on
	// purpose: the broker routes the capability to a free worker by
	// consumer priority, so the runner does not decide a location.
	if task.Capability != "" {
		return r.runCapability(ctx, job, card, task, inProgressID, doneID)
	}

	// Placement: the executor model from the card's model: label (the
	// planner's choice), the run: label as override.
	modelID := job.ModelID()
	m, ok := r.registryModel(modelID)
	switch {
	case modelID == "":
		r.blockCard(ctx, projectID, card.ID, "no model: label on the card — the plan chose no executor")
		return Result{Blocked: true, Summary: "no model: label"}, nil
	case !ok:
		r.blockCard(ctx, projectID, card.ID, fmt.Sprintf("executor model %s is not in the registry", modelID))
		return Result{Blocked: true, Summary: "unknown model " + modelID}, nil
	case !m.Enabled || !hasRole(m.Roles, "executor"):
		r.blockCard(ctx, projectID, card.ID, fmt.Sprintf("executor model %s is not an enabled executor", modelID))
		return Result{Blocked: true, Summary: "model " + modelID + " is not an enabled executor"}, nil
	}
	live, err := r.Live(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("live placement state: %w", err)
	}
	dec := placement.Decide(m, job.RunOverride(), live)
	if dec.Blocked {
		r.blockCard(ctx, projectID, card.ID, dec.Reason)
		return Result{Blocked: true, Summary: dec.Reason}, nil
	}
	if dec.Location == placement.Cloud {
		// Cloud execution is not wired yet (C3): leave the card in
		// Ready; the bridge re-triggers the job on the next catch-up.
		if !r.hasComment(ctx, projectID, card.ID, "cloud execution is not wired yet") {
			r.comment(ctx, projectID, card.ID, "taskrunner: cloud execution is not wired yet (C3) — the card stays in Ready")
		}
		return Result{Waiting: true, Summary: "cloud execution not wired yet (C3)"}, nil
	}

	// Move the card to In progress before any work happens (the PLAN.md
	// "state changes … written to Plane each step" contract).
	if err := r.setState(ctx, projectID, card.ID, inProgressID); err != nil {
		return Result{}, fmt.Errorf("move card to %s: %w", StateInProgress, err)
	}

	// Branch names: the task branch is "epic-<code>/task-<n>" (PLAN.md).
	// The epic branch must not be "epic-<code>" — git would refuse the
	// task branch (refs/heads/epic-<code> exists, so it cannot become a
	// directory). "epics/<code>" is prefix-free against it.
	epicCode := fmt.Sprintf("%s-%d", job.Board.Project, epic.SequenceID)
	epicBranch := "epics/" + epicCode
	taskBranch := "epic-" + epicCode + "/task-" + fmt.Sprint(card.SequenceID)
	// Worktree directory names (not refs — no prefix rules apply).
	epicWtName := "epic-" + epicCode
	epicWt := filepath.Join(r.Repo, ".worktrees", epicWtName)
	taskWt := filepath.Join(r.Repo, ".worktrees", epicWtName+"--task-"+fmt.Sprint(card.SequenceID))

	// One run at a time per epic: two tasks of the same epic (a re-plan
	// that mixes locations) must not touch the epic worktree in
	// parallel. Different epics run in parallel — that is what the
	// location queues are for.
	lock, err := r.epicLock(r.Repo, epicWtName)
	if err != nil {
		return Result{}, fmt.Errorf("epic lock: %w", err)
	}
	defer lock.Close()

	if err := r.ensureEpicWorktree(ctx, epicBranch, epicWt); err != nil {
		return Result{}, err
	}
	plan := r.planPage(ctx, projectID, epic)
	notes := r.notesTail(filepath.Join(epicWt, r.NotesFile))

	var prev string
	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		r.comment(ctx, projectID, card.ID, fmt.Sprintf("%s: running on %s — branch %s, attempt %d/%d",
			modelID, dec.Location, taskBranch, attempt, r.MaxAttempts))
		// Attempt 1 starts clean (a leftover branch from a crashed run
		// is dropped); later attempts keep the harness's partial edits
		// and build on them with the error fed back.
		if err := r.setupTaskWorktree(ctx, taskBranch, taskWt, epicBranch, attempt == 1); err != nil {
			return Result{}, err
		}
		prompt := r.prompt(epic, card, task, plan, notes, attempt, prev)
		herr := r.Harness.Run(ctx, taskWt, prompt, task.Files)
		passed, output := r.Checks.Check(ctx, taskWt, task.AcceptanceCheck)
		if herr == nil && passed {
			return r.finish(ctx, job, card, epicBranch, epicWt, taskWt, taskBranch, modelID, doneID, attempt)
		}
		if herr != nil {
			output = fmt.Sprintf("harness: %v\n%s", herr, output)
		}
		prev = output
		r.comment(ctx, projectID, card.ID, fmt.Sprintf("%s: attempt %d/%d — check failed:\n%s",
			modelID, attempt, r.MaxAttempts, tail(output, 1500)))
	}
	r.blockCard(ctx, projectID, card.ID, fmt.Sprintf(
		"%s: blocked after %d attempts; branch %s left for inspection. Last check output:\n%s",
		modelID, r.MaxAttempts, taskBranch, tail(prev, 1500)))
	return Result{Blocked: true, Summary: fmt.Sprintf("failed after %d attempts", r.MaxAttempts), Error: tail(prev, 4000)}, nil
}

// runCapability executes a card whose executor is a capability (plan §3.7):
// move it to In progress, submit it to cap.<capability> (reply-to
// flow.<card-id>, corr = the card id), wait for the Result, and settle the
// card from the Result's status — ok advances it to Done, anything else
// Blocks it with the reason. No placement, no harness: the broker routes the
// request to a free worker by consumer priority and the Result is the whole
// handoff, so a capability card never runs an LLM round.
func (r *Runner) runCapability(ctx context.Context, job *Job, card *plane.WorkItem, task *planworker.Task, inProgressID, doneID string) (Result, error) {
	projectID := job.Board.ProjectID
	if r.Caps == nil {
		r.blockCard(ctx, projectID, card.ID, "capability "+task.Capability+" requested but no capability client is wired (broker not configured)")
		return Result{Blocked: true, Summary: "no capability client wired"}, nil
	}
	if !queue.ValidCapabilityName(task.Capability) {
		r.blockCard(ctx, projectID, card.ID, "capability name "+task.Capability+" is not valid (want a dotted lowercase-alphanumeric path)")
		return Result{Blocked: true, Summary: "invalid capability name"}, nil
	}
	// Move the card to In progress before any work (the PLAN.md "state
	// changes ... written to Plane each step" contract).
	if err := r.setState(ctx, projectID, card.ID, inProgressID); err != nil {
		return Result{}, fmt.Errorf("move card to %s: %w", StateInProgress, err)
	}
	r.comment(ctx, projectID, card.ID, "taskrunner: submitted to "+queue.CapQueueName(task.Capability)+", waiting for the Result (no LLM round)")
	cctx, cancel := context.WithTimeout(ctx, r.capabilityTimeout())
	defer cancel()
	res, err := r.Caps.Run(cctx, task.Capability, card.ID, capabilityInput(task))
	if err != nil {
		// A shutdown (ctx cancelled) aborts the run; the store entry settles
		// it as failed and the card is left In progress, exactly as a
		// mid-harness crash would. A submit failure is definitive — block the
		// card with the reason so a human can see it and reset it (Blocked ->
		// Ready re-triggers).
		if errors.Is(err, context.Canceled) {
			return Result{}, err
		}
		r.blockCard(ctx, projectID, card.ID, "capability submit failed: "+err.Error())
		return Result{Blocked: true, Summary: "capability submit failed", Error: err.Error()}, nil
	}
	if res.Status == queue.ResultOK {
		r.comment(ctx, projectID, card.ID, capabilityResultComment(task, res))
		// The capability's work is done; a flaky state move must not mark the
		// run failed (mirrors finish's retry for a normal card).
		if stateErr := r.moveToDone(ctx, projectID, card.ID, doneID); stateErr != nil {
			r.logf("move capability card to Done: %v", stateErr)
		}
		return Result{Passed: true, Summary: "capability " + task.Capability + " ok: " + res.Summary}, nil
	}
	// error / timeout / cancelled: settle as Blocked with the reason.
	reason := res.Error
	if reason == "" {
		reason = "status " + res.Status
	}
	r.blockCard(ctx, projectID, card.ID, fmt.Sprintf("capability %s ended %s: %s", task.Capability, res.Status, reason))
	return Result{Blocked: true, Summary: fmt.Sprintf("capability %s %s", task.Capability, res.Status), Error: reason}, nil
}

// capabilityTimeout bounds the wait for a capability's Result, defaulting to
// DefaultCapabilityTimeout.
func (r *Runner) capabilityTimeout() time.Duration {
	if r.CapabilityTimeout > 0 {
		return r.CapabilityTimeout
	}
	return DefaultCapabilityTimeout
}

// capabilityInput builds the input the capability worker reads, from the
// card's description (the decompose prompt puts the capability's input
// there — what to generate, the file to describe, the clip to transcribe).
// The description is passed under the keys the common capabilities read
// (prompt for the generate family, description as the generic fallback); a
// capability that needs a specific path or verb reads its own primary key.
func capabilityInput(task *planworker.Task) map[string]any {
	return map[string]any{
		"prompt":      task.Description,
		"description": task.Description,
	}
}

// capabilityResultComment renders the Result as the card's "the result lands
// on the card" comment: the status, the summary, the artifact references (not
// the bytes — invariant 7), and the worker that produced it.
func capabilityResultComment(task *planworker.Task, res queue.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "taskrunner: capability %s — %s\n", task.Capability, res.Status)
	if res.Summary != "" {
		b.WriteString("summary: " + res.Summary + "\n")
	}
	if len(res.Artifacts) > 0 {
		b.WriteString("artifacts: " + strings.Join(res.Artifacts, ", ") + "\n")
	}
	if res.Worker != "" {
		b.WriteString("worker: " + res.Worker + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// moveToDone sets the card to Done, retrying a few times: the capability's
// work is already done, so a flaky state move is retried rather than
// surfacing as a failed run (the same reasoning as finish's Done move).
func (r *Runner) moveToDone(ctx context.Context, projectID, itemID, doneID string) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = r.setState(ctx, projectID, itemID, doneID); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return err
}

// finish commits the task worktree, merges it into the epic branch,
// records NOTES.md, pushes, cleans up, and moves the card to Done.
func (r *Runner) finish(ctx context.Context, job *Job, card *plane.WorkItem,
	epicBranch, epicWt, taskWt, taskBranch, modelID, doneID string, attempt int) (Result, error) {
	sha, err := r.commitTask(ctx, taskWt, card.SequenceID, card.Name)
	if err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	if out, err := gitRun(ctx, epicWt, "merge", "--no-ff", taskBranch,
		"-m", fmt.Sprintf("task-%d: %s", card.SequenceID, card.Name)); err != nil {
		return Result{}, fmt.Errorf("merge into %s: %v (%s)", epicBranch, err, out)
	}
	r.appendNote(epicWt, job, card, sha)
	r.pushEpic(epicBranch)
	if err := r.cleanupTask(ctx, taskWt, taskBranch); err != nil {
		r.logf("cleanup task worktree: %v (the merge already landed)", err)
	}
	// The work is merged; a flaky state move must not mark the run
	// failed (the card is not Ready, so nothing would re-trigger it).
	var stateErr error
	for i := 0; i < 3; i++ {
		if stateErr = r.setState(ctx, job.Board.ProjectID, card.ID, doneID); stateErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if stateErr != nil {
		r.logf("move card to Done: %v", stateErr)
	}
	r.comment(ctx, job.Board.ProjectID, card.ID, fmt.Sprintf("%s: done (attempt %d/%d) — commit %s, merged into %s",
		modelID, attempt, r.MaxAttempts, sha[:7], epicBranch))
	return Result{Passed: true, Summary: fmt.Sprintf("done — commit %s, merged into %s", sha[:7], epicBranch)}, nil
}

// appendNote adds the one-line NOTES.md entry (PLAN.md step 7) and
// commits it on the epic branch. A missing file is created.
func (r *Runner) appendNote(epicWt string, job *Job, card *plane.WorkItem, sha string) {
	path := filepath.Join(epicWt, r.NotesFile)
	line := fmt.Sprintf("- %s-%d: %s — done %s (commit %s)\n",
		job.Board.Project, card.SequenceID, card.Name, time.Now().UTC().Format("2006-01-02"), sha[:7])
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.logf("notes: open %s: %v", r.NotesFile, err)
		return
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		r.logf("notes: write: %v", err)
		return
	}
	f.Close()
	if out, err := gitRun(context.Background(), epicWt, "add", r.NotesFile); err != nil {
		r.logf("notes: git add: %v (%s)", err, out)
		return
	}
	if out, err := gitRun(context.Background(), epicWt, "commit", "-m",
		fmt.Sprintf("notes: %s-%d done", job.Board.Project, card.SequenceID)); err != nil {
		r.logf("notes: git commit: %v (%s)", err, out)
	}
}

// pushEpic pushes the epic branch to origin when the repo has one
// (best effort: the toy repos in the live test have no remote).
func (r *Runner) pushEpic(epicBranch string) {
	if _, err := gitRun(context.Background(), r.Repo, "remote", "get-url", "origin"); err != nil {
		return
	}
	if out, err := gitRun(context.Background(), r.Repo, "push", "origin", epicBranch); err != nil {
		r.logf("push %s: %v (%s)", epicBranch, err, out)
	}
}

func (r *Runner) commitTask(ctx context.Context, taskWt string, seq int, title string) (string, error) {
	if out, err := gitRun(ctx, taskWt, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %v (%s)", err, out)
	}
	status, _ := gitRun(ctx, taskWt, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return "", fmt.Errorf("harness made no changes")
	}
	if out, err := gitRun(ctx, taskWt, "commit", "-m", fmt.Sprintf("task-%d: %s", seq, title)); err != nil {
		return "", fmt.Errorf("git commit: %v (%s)", err, out)
	}
	sha, err := gitRun(ctx, taskWt, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse: %v", err)
	}
	return strings.TrimSpace(sha), nil
}

func (r *Runner) cleanupTask(ctx context.Context, taskWt, taskBranch string) error {
	if err := os.RemoveAll(taskWt); err != nil {
		return err
	}
	if _, err := gitRun(ctx, r.Repo, "worktree", "prune"); err != nil {
		return err
	}
	if _, err := gitRun(ctx, r.Repo, "branch", "-D", taskBranch); err != nil {
		return fmt.Errorf("delete %s: %v", taskBranch, err)
	}
	return nil
}

// depState is one unmet dependency: its card id plus a detail string
// (what the human looking at the comment needs to act on).
type depState struct {
	ID     string
	Detail string
}

func details(ds []depState) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Detail
	}
	return out
}

// missingDeps reports the deps of the card that are not Done. Dep ids
// name sibling cards (their plan-task: markers), resolved across the
// epic's sub-items.
func (r *Runner) missingDeps(epic *plane.WorkItem, deps []string, items []plane.WorkItem, doneID string, stateByID map[string]string) []depState {
	byTask := map[string]*plane.WorkItem{}
	for i := range items {
		it := &items[i]
		if it.Parent != epic.ID || it.ID == epic.ID {
			continue
		}
		t, err := planworker.ParseTaskCard(it.DescriptionHTML)
		if err != nil {
			continue // a non-card sub-item of the epic
		}
		byTask[t.ID] = it
	}
	var missing []depState
	for _, d := range deps {
		sib, ok := byTask[d]
		if !ok {
			missing = append(missing, depState{ID: d, Detail: d + " (no such card)"})
			continue
		}
		if sib.State != doneID {
			missing = append(missing, depState{ID: d, Detail: fmt.Sprintf("%s (%s, in %s)", d, sib.Name, stateByID[sib.State])})
		}
	}
	return missing
}

// planPage fetches the epic's plan page (B4 names it "Plan: <epic name>")
// and reduces it to plain text. A missing page is not an error — the card
// stands on its own.
func (r *Runner) planPage(ctx context.Context, projectID string, epic *plane.WorkItem) string {
	pages, err := r.Board.ListPages(ctx, projectID)
	if err != nil {
		r.logf("list pages: %v", err)
		return ""
	}
	name := "Plan: " + epic.Name
	for i := range pages {
		if pages[i].Name != name {
			continue
		}
		pg, err := r.Board.GetPage(ctx, projectID, pages[i].ID)
		if err != nil {
			r.logf("get page: %v", err)
			return ""
		}
		body := strings.TrimSpace(pg.DescriptionHTML)
		body = strings.TrimPrefix(body, "<pre>")
		body = strings.TrimSuffix(body, "</pre>")
		return tail(html.UnescapeString(body), 6000)
	}
	return ""
}

// notesTail is the last ~300 lines of the project's NOTES.md — the recent
// context the PLAN.md wants in the executor's prompt. Read from the epic
// worktree: earlier tasks of the same epic committed their notes there,
// and the main worktree does not have them yet.
func (r *Runner) notesTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > 300 {
		lines = lines[len(lines)-300:]
	}
	return tail(strings.Join(lines, "\n"), 6000)
}

// prompt assembles the executor's context: the card, the file list, the
// plan, the recent notes, and — from attempt two on — the previous
// attempt's check output (the "feed the errors back" retry contract).
func (r *Runner) prompt(epic, card *plane.WorkItem, task *planworker.Task,
	plan, notes string, attempt int, prev string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Complete exactly one task card from an approved plan.\n\n")
	fmt.Fprintf(&b, "Epic: %s\nTask %d: %s\n\n", epic.Name, card.SequenceID, card.Name)
	fmt.Fprintf(&b, "What to build:\n%s\n\n", task.Description)
	if len(task.Files) > 0 {
		b.WriteString("Files (create any that do not exist yet):\n")
		for _, f := range task.Files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Files: none listed — keep the change minimal.\n\n")
	}
	fmt.Fprintf(&b, "Acceptance check (the runner runs it; make it pass):\n%s\n", task.AcceptanceCheck)
	if plan != "" {
		fmt.Fprintf(&b, "\nPlan for this epic (from the board):\n%s\n", plan)
	}
	if notes != "" {
		fmt.Fprintf(&b, "\nProject notes (recent tail):\n%s\n", notes)
	}
	if attempt > 1 && prev != "" {
		fmt.Fprintf(&b, "\nThe previous attempt failed. Its check output:\n%s\nFix the failures and finish the task.\n", tail(prev, 3000))
	}
	b.WriteString("\nEdit the files directly. Do not commit; the runner commits when the check passes.\n")
	return b.String()
}

// ensureEpicWorktree makes sure the epic branch exists and has a
// worktree, creating the branch from origin/main (or main) on first use.
func (r *Runner) ensureEpicWorktree(ctx context.Context, epicBranch, epicWt string) error {
	if _, err := gitRun(ctx, r.Repo, "rev-parse", "--is-inside-work-tree"); err != nil {
		return fmt.Errorf("%s is not a git repository: %v", r.Repo, err)
	}
	r.excludeWorktrees()
	if _, err := gitRun(ctx, r.Repo, "worktree", "prune"); err != nil {
		return err
	}
	if !refExists(ctx, r.Repo, "refs/heads/"+epicBranch) {
		base := "origin/main"
		if !refExists(ctx, r.Repo, "refs/remotes/origin/main") {
			base = "main"
		}
		if out, err := gitRun(ctx, r.Repo, "worktree", "add", "-b", epicBranch, epicWt, base); err != nil {
			return fmt.Errorf("create %s from %s: %v (%s)", epicBranch, base, err, out)
		}
		return nil
	}
	// The epic branch exists. A previous run of this epic (blocked or
	// crashed) leaves its worktree attached for inspection, and re-runs
	// of a blocked card are the designed recovery path (Blocked →
	// Ready → the bridge re-triggers). git worktree add on an existing
	// path is fatal (exit 128), so keep the worktree that is already on
	// the right branch instead of failing.
	if branch, attached, err := worktreeBranchAt(ctx, r.Repo, epicWt); err != nil {
		return fmt.Errorf("worktree list: %v", err)
	} else if attached && branch != epicBranch {
		return fmt.Errorf("attach %s: the path is a worktree of %s, want %s", epicWt, branch, epicBranch)
	} else if !attached {
		if out, err := gitRun(ctx, r.Repo, "worktree", "add", epicWt, epicBranch); err != nil {
			return fmt.Errorf("attach %s: %v (%s)", epicWt, err, out)
		}
	}
	return nil
}

// worktreeBranchAt reports the branch checked out in the worktree at
// path (attached=false when path is not a registered worktree).
func worktreeBranchAt(ctx context.Context, repo, path string) (branch string, attached bool, err error) {
	out, err := gitRun(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	inTarget := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			inTarget = strings.TrimPrefix(line, "worktree ") == path
			continue
		}
		if inTarget && strings.HasPrefix(line, "branch ") {
			b := strings.TrimPrefix(line, "branch ")
			return strings.TrimPrefix(b, "refs/heads/"), true, nil
		}
	}
	return "", false, nil
}

// setupTaskWorktree points taskWt at taskBranch. fresh (attempt one, and
// the recovery case for a crashed run) drops any leftover branch/worktree
// and starts from the epic branch; fresh=false reuses what an earlier
// attempt left, so the harness builds on its partial edits.
func (r *Runner) setupTaskWorktree(ctx context.Context, taskBranch, taskWt, epicBranch string, fresh bool) error {
	if fresh {
		// A blocked or crashed run leaves the task worktree registered
		// and its branch checked out there. RemoveAll drops the
		// directory but not the registration, so prune before dropping
		// the branch — branch -D refuses a branch that git still
		// considers checked out.
		if _, err := gitRun(ctx, r.Repo, "worktree", "prune"); err != nil {
			return err
		}
		if _, err := os.Stat(taskWt); err == nil {
			if err := os.RemoveAll(taskWt); err != nil {
				return fmt.Errorf("remove stale %s: %v", taskWt, err)
			}
			if _, err := gitRun(ctx, r.Repo, "worktree", "prune"); err != nil {
				return err
			}
		}
		if refExists(ctx, r.Repo, "refs/heads/"+taskBranch) {
			if _, err := gitRun(ctx, r.Repo, "branch", "-D", taskBranch); err != nil {
				return fmt.Errorf("delete stale %s: %v", taskBranch, err)
			}
		}
	} else if _, err := os.Stat(taskWt); err != nil {
		// A crashed run between attempts (the process died) leaves the
		// branch without its worktree: prune the stale registration,
		// then re-attach.
		if _, err := gitRun(ctx, r.Repo, "worktree", "prune"); err != nil {
			return err
		}
		if out, err := gitRun(ctx, r.Repo, "worktree", "add", taskWt, taskBranch); err != nil {
			return fmt.Errorf("re-attach %s: %v (%s)", taskWt, err, out)
		}
	}
	if refExists(ctx, r.Repo, "refs/heads/"+taskBranch) && !fresh {
		return nil // the worktree is attached to the branch
	}
	if out, err := gitRun(ctx, r.Repo, "worktree", "add", "-b", taskBranch, taskWt, epicBranch); err != nil {
		return fmt.Errorf("create %s from %s: %v (%s)", taskBranch, epicBranch, err, out)
	}
	return nil
}

// excludeWorktrees keeps .worktrees/ out of the repo's git status (the
// runner's scratch space is not part of any card).
func (r *Runner) excludeWorktrees() {
	excl := filepath.Join(r.Repo, ".git", "info", "exclude")
	b, err := os.ReadFile(excl)
	if err == nil && strings.Contains(string(b), ".worktrees/") {
		return
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintln(f, ".worktrees/")
	f.Close()
}

// epicLock blocks until the per-epic run lock is held.
func (r *Runner) epicLock(repo, epicWtName string) (*os.File, error) {
	dir := filepath.Join(repo, ".worktrees", "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, epicWtName+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// blockCard moves the card to Blocked and leaves the reason as a comment.
// A failed move is logged, not fatal: the comment carries the reason and
// the store entry settles the job either way.
func (r *Runner) blockCard(ctx context.Context, projectID, itemID, reason string) {
	r.comment(ctx, projectID, itemID, "taskrunner: blocked — "+reason)
	states, err := r.Board.ListStates(ctx, projectID)
	if err != nil {
		r.logf("list states for block: %v", err)
		return
	}
	for _, s := range states {
		if strings.EqualFold(s.Name, StateBlocked) {
			if _, err := r.Board.SetState(ctx, projectID, itemID, s.ID); err != nil {
				r.logf("move card to Blocked: %v", err)
			}
			return
		}
	}
	r.logf("project %s has no %q state", projectID, StateBlocked)
}

func (r *Runner) setState(ctx context.Context, projectID, itemID, stateID string) error {
	_, err := r.Board.SetState(ctx, projectID, itemID, stateID)
	return err
}

// comment posts a board comment; failures are logged and dropped (a
// comment is a courtesy — the state move and the store entry are the
// contract).
func (r *Runner) comment(ctx context.Context, projectID, itemID, text string) {
	if _, err := r.Board.AddComment(ctx, projectID, itemID, text); err != nil {
		r.logf("comment: %v", err)
	}
}

// hasComment reports whether any comment on the item contains needle —
// the dedupe for the "waiting on" comment, so a card that waits across
// many catch-up re-triggers is not spammed.
func (r *Runner) hasComment(ctx context.Context, projectID, itemID, needle string) bool {
	comments, err := r.Board.ListComments(ctx, projectID, itemID)
	if err != nil {
		return false
	}
	for _, c := range comments {
		if strings.Contains(c.CommentHTML, needle) {
			return true
		}
	}
	return false
}

func (r *Runner) registryModel(id string) (registry.Model, bool) {
	if r.Registry == nil {
		return registry.Model{}, false
	}
	for i := range r.Registry.Models {
		if r.Registry.Models[i].ID == id {
			return r.Registry.Models[i], true
		}
	}
	return registry.Model{}, false
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func (r *Runner) defaults() {
	if r.NotesFile == "" {
		r.NotesFile = DefaultNotesFile
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Info(fmt.Sprintf(format, args...))
	}
}

// gitRun runs git in dir and returns its trimmed output.
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// refExists reports whether the git ref (e.g. "refs/heads/main") exists.
func refExists(ctx context.Context, repo, ref string) bool {
	_, err := gitRun(ctx, repo, "rev-parse", "--verify", ref)
	return err == nil
}
