package taskrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// fakeBoard is an in-memory BoardAPI: states, work items, comments, pages.
type fakeBoard struct {
	mu       sync.Mutex
	states   []plane.State
	items    map[string]plane.WorkItem
	comments map[string][]plane.Comment
	pages    []plane.Page
	commentN int
}

func newFakeBoard() *fakeBoard {
	return &fakeBoard{
		states: []plane.State{
			{ID: "s-ready", Name: "Ready"},
			{ID: "s-progress", Name: "In progress"},
			{ID: "s-blocked", Name: "Blocked"},
			{ID: "s-done", Name: "Done"},
		},
		items:    map[string]plane.WorkItem{},
		comments: map[string][]plane.Comment{},
	}
}

func (b *fakeBoard) add(item plane.WorkItem) {
	b.items[item.ID] = item
}

func (b *fakeBoard) ListStates(ctx context.Context, projectID string) ([]plane.State, error) {
	return b.states, nil
}

func (b *fakeBoard) ListWorkItems(ctx context.Context, projectID string) ([]plane.WorkItem, error) {
	out := make([]plane.WorkItem, 0, len(b.items))
	for _, it := range b.items {
		out = append(out, it)
	}
	return out, nil
}

func (b *fakeBoard) GetWorkItem(ctx context.Context, projectID, itemID string) (*plane.WorkItem, error) {
	it, ok := b.items[itemID]
	if !ok {
		return nil, fmt.Errorf("no item %s", itemID)
	}
	return &it, nil
}

func (b *fakeBoard) SetState(ctx context.Context, projectID, itemID, stateID string) (*plane.WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	it, ok := b.items[itemID]
	if !ok {
		return nil, fmt.Errorf("no item %s", itemID)
	}
	it.State = stateID
	b.items[itemID] = it
	return &it, nil
}

func (b *fakeBoard) AddComment(ctx context.Context, projectID, itemID, html string) (*plane.Comment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commentN++
	c := plane.Comment{ID: fmt.Sprintf("c%d", b.commentN), CommentHTML: html}
	b.comments[itemID] = append(b.comments[itemID], c)
	return &c, nil
}

func (b *fakeBoard) ListComments(ctx context.Context, projectID, itemID string) ([]plane.Comment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]plane.Comment{}, b.comments[itemID]...), nil
}

func (b *fakeBoard) ListPages(ctx context.Context, projectID string) ([]plane.Page, error) {
	return append([]plane.Page{}, b.pages...), nil
}

func (b *fakeBoard) GetPage(ctx context.Context, projectID, pageID string) (*plane.Page, error) {
	for i := range b.pages {
		if b.pages[i].ID == pageID {
			p := b.pages[i]
			return &p, nil
		}
	}
	return nil, fmt.Errorf("no page %s", pageID)
}

// cardHTML builds a task-card description in the B4 format.
func cardHTML(id, what, files, deps, check, size, model string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>plan-task: %s</p>\n", id)
	fmt.Fprintf(&b, "<p><strong>What to build:</strong> %s</p>\n", what)
	if files != "" {
		fmt.Fprintf(&b, "<p><strong>Files:</strong> %s</p>\n", files)
	}
	if deps != "" {
		fmt.Fprintf(&b, "<p><strong>Blocked by:</strong> %s</p>\n", deps)
	}
	fmt.Fprintf(&b, "<p><strong>Acceptance check:</strong> %s</p>\n", check)
	fmt.Fprintf(&b, "<p><strong>Size:</strong> %s &middot; <strong>Executor:</strong> model:%s</p>\n", size, model)
	return b.String()
}

// fakeHarness writes its file on the passAfter-th call; earlier calls
// write a broken version (so the check fails until then).
type fakeHarness struct {
	mu        sync.Mutex
	passAfter int
	writes    int
	good, bad string
	file      string
}

func (h *fakeHarness) Run(ctx context.Context, dir, prompt string, files []string) error {
	h.mu.Lock()
	h.writes++
	n := h.writes
	h.mu.Unlock()
	content := h.bad
	if n >= h.passAfter {
		content = h.good
	}
	return os.WriteFile(filepath.Join(dir, h.file), []byte(content), 0o644)
}

// contentChecker passes when the file contains want.
type contentChecker struct {
	file, want string
}

func (c contentChecker) Check(ctx context.Context, dir, acceptance string) (bool, string) {
	b, err := os.ReadFile(filepath.Join(dir, c.file))
	if err != nil {
		return false, "acceptance: no file: " + err.Error()
	}
	if strings.Contains(string(b), c.want) {
		return true, "acceptance: ok"
	}
	return false, fmt.Sprintf("acceptance: %s does not contain %q (content: %s)", c.file, c.want, string(b))
}

func testRegistry() *registry.File {
	return &registry.File{Models: []registry.Model{
		{ID: "local-27b", Roles: []string{"executor"}, Enabled: true,
			HomeLocation: string(placement.GPU), FallbackLocation: string(placement.CPU), VRAMGB: 20},
		{ID: "cloud-model", Roles: []string{"executor"}, Enabled: true,
			HomeLocation: string(placement.Cloud)},
	}}
}

// testLive: 1 GiB VRAM free, plenty of RAM → the 27B falls back to cpu.
func testLive(ctx context.Context) (placement.State, error) {
	return placement.State{FreeVRAMGB: 1, FreeRAMGB: 100}, nil
}

// newRepo makes a throwaway git repo with a NOTES.md at the default path.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=runner-test", "GIT_AUTHOR_EMAIL=test@local",
			"GIT_COMMITTER_NAME=runner-test", "GIT_COMMITTER_EMAIL=test@local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "docs", "work-board"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "work-board", "NOTES.md"), []byte("# Notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "init")
	return dir
}

func testRunner(t *testing.T, board *fakeBoard, harness Harness, checks Checker) *Runner {
	t.Helper()
	return &Runner{
		Board:       board,
		Registry:    testRegistry(),
		Repo:        newRepo(t),
		Harness:     harness,
		Checks:      checks,
		Live:        testLive,
		MaxAttempts: 3,
	}
}

func runJob(t *testing.T, r *Runner, cardID, parent string, labels ...string) (Result, error) {
	t.Helper()
	job := &Job{
		Trigger: TriggerRun,
		Board:   Board{Project: "TST", ProjectID: "proj1"},
		Item:    Item{ID: cardID, Name: "health endpoint", Parent: parent, Labels: labels},
	}
	return r.Run(context.Background(), job)
}

// TestRunPasses: the happy path — the card goes Done, the task branch is
// committed and merged (no-ff) into the epic branch, NOTES.md gains a
// line, and the task worktree and branch are cleaned up.
func TestRunPasses(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2, Labels: []string{"epic"}})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 1, good: "print('ok')", bad: "bad", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed || res.Waiting || res.Blocked {
		t.Fatalf("Result = %+v, want Passed", res)
	}

	if got := board.items["card1"].State; got != "s-done" {
		t.Errorf("card state = %s, want s-done", got)
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	var joined string
	for _, c := range comments {
		joined += c.CommentHTML + "\n"
	}
	for _, want := range []string{"running on cpu — branch epic-TST-2/task-3, attempt 1/3", "done (attempt 1/3)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("comments missing %q:\n%s", want, joined)
		}
	}

	repo := r.Repo
	gitOut := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	log := gitOut("log", "--oneline", "epics/TST-2")
	if !strings.Contains(log, "task-3: health endpoint") {
		t.Errorf("epic branch log missing the task commit:\n%s", log)
	}
	if gitOut("rev-list", "--count", "--merges", "epics/TST-2") != "1" {
		t.Errorf("want exactly one merge commit on epics/TST-2:\n%s", log)
	}
	notes := gitOut("show", "epics/TST-2:docs/work-board/NOTES.md")
	if !strings.Contains(notes, "TST-3: health endpoint — done") {
		t.Errorf("NOTES.md missing the done line:\n%s", notes)
	}
	if gitOut("branch", "--list", "epic-TST-2/task-3") != "" {
		t.Error("task branch not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "epic-TST-2--task-3")); !os.IsNotExist(err) {
		t.Error("task worktree not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "epic-TST-2")); err != nil {
		t.Errorf("epic worktree should stay for the next task: %v", err)
	}
}

// TestRunLegacyStateNames: the projects migrated from AAB carry Plane's
// default "In Progress" (capital P), not the plane-epic standard "In
// progress". The runner matches state names case-insensitively (like the
// bridge) or every migrated project fails with "no In progress state".
func TestRunLegacyStateNames(t *testing.T) {
	board := newFakeBoard()
	board.states = []plane.State{
		{ID: "s-ready", Name: "Ready"},
		{ID: "s-progress", Name: "In Progress"},
		{ID: "s-blocked", Name: "Blocked"},
		{ID: "s-done", Name: "Done"},
	}
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2, Labels: []string{"epic"}})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 1, good: "print('ok')", bad: "bad", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed {
		t.Fatalf("Result = %+v, want Passed", res)
	}
	if got := board.items["card1"].State; got != "s-done" {
		t.Errorf("card state = %s, want s-done", got)
	}
}

// TestRunRetriesThenPasses: attempt one fails the check, attempt two
// builds on it and passes.
func TestRunRetriesThenPasses(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 2, good: "print('ok')", bad: "bad", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed {
		t.Fatalf("Result = %+v, want Passed", res)
	}
	if harness.writes != 2 {
		t.Errorf("harness ran %d times, want 2", harness.writes)
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	var joined string
	for _, c := range comments {
		joined += c.CommentHTML + "\n"
	}
	for _, want := range []string{"attempt 1/3 — check failed", "done (attempt 2/3)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("comments missing %q:\n%s", want, joined)
		}
	}
	// Attempt two reused attempt one's worktree (fresh=false): the file
	// the harness wrote in attempt one was still there to be rewritten.
	if n := strings.Count(joined, "— check failed"); n != 1 {
		t.Errorf("%d failure comments posted, want 1:\n%s", n, joined)
	}
}

// TestRunBlocksAfterMaxAttempts: three failures → the card is Blocked,
// the task branch is left for inspection.
func TestRunBlocksAfterMaxAttempts(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 99, good: "", bad: "bad", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("Result = %+v, want Blocked", res)
	}
	if got := board.items["card1"].State; got != "s-blocked" {
		t.Errorf("card state = %s, want s-blocked", got)
	}
	out, _ := exec.Command("git", "-C", r.Repo, "branch", "--list", "epic-TST-2/task-3").CombinedOutput()
	if strings.TrimSpace(string(out)) == "" {
		t.Error("task branch should be left for inspection after three failures")
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	var joined string
	for _, c := range comments {
		joined += c.CommentHTML + "\n"
	}
	if !strings.Contains(joined, "blocked after 3 attempts") {
		t.Errorf("comments missing the blocked note:\n%s", joined)
	}
}

// TestRerunBlockedEpicReusesWorktree: a blocked run leaves the epic
// worktree attached; the designed recovery path (Blocked → Ready → the
// bridge re-triggers) must not die on git worktree add over the existing
// path (fatal, exit 128). The re-run reuses the attached worktree and
// finishes the card.
func TestRerunBlockedEpicReusesWorktree(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 99, good: "", bad: "bad", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("first run = %+v, want Blocked", res)
	}
	if _, err := os.Stat(filepath.Join(r.Repo, ".worktrees", "epic-TST-2")); err != nil {
		t.Fatalf("epic worktree missing after blocked run: %v", err)
	}

	// The fix lands, the card goes back to Ready, the bridge re-triggers.
	harness.passAfter = 1
	harness.good = "ok"
	if _, err := board.SetState(context.Background(), "proj1", "card1", "s-ready"); err != nil {
		t.Fatalf("reset card to Ready: %v", err)
	}
	res, err = runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if !res.Passed {
		t.Fatalf("re-run = %+v, want Passed (epic worktree already attached)", res)
	}
	if got := board.items["card1"].State; got != "s-done" {
		t.Errorf("card state = %s, want s-done", got)
	}
}

// TestRunWaitsOnDeps: a dep that is not Done → Waiting, one "waiting on"
// comment, and a second call (the bridge's re-trigger) posts no
// duplicate.
func TestRunWaitsOnDeps(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card-base", Name: "base", State: "s-ready", Parent: "epic1", SequenceID: 4,
		DescriptionHTML: cardHTML("base", "the base", "", "", "true", "S", "local-27b"),
	})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "base", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Waiting {
		t.Fatalf("Result = %+v, want Waiting", res)
	}
	if got := board.items["card1"].State; got != "s-ready" {
		t.Errorf("card state = %s, want s-ready (untouched)", got)
	}
	if harness.writes != 0 {
		t.Errorf("harness ran %d times while waiting, want 0", harness.writes)
	}
	n1, _ := board.ListComments(context.Background(), "proj1", "card1")
	if len(n1) != 1 || !strings.Contains(n1[0].CommentHTML, "waiting on: base") {
		t.Fatalf("first run comments = %+v, want one waiting-on comment", n1)
	}

	// The bridge re-triggers while the card is Ready: no duplicate.
	if res, err = runJob(t, r, "card1", "epic1", "model:local-27b"); err != nil || !res.Waiting {
		t.Fatalf("second Run = %+v, %v", res, err)
	}
	n2, _ := board.ListComments(context.Background(), "proj1", "card1")
	if len(n2) != 1 {
		t.Errorf("second run posted %d comments, want 0 new", len(n2)-1)
	}

	// The dep finishing unblocks the run.
	board.add(plane.WorkItem{ID: "card-base", Name: "base", State: "s-done", Parent: "epic1", SequenceID: 4,
		DescriptionHTML: cardHTML("base", "the base", "", "", "true", "S", "local-27b")})
	res, err = runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run after dep done: %v", err)
	}
	if !res.Passed {
		t.Fatalf("Result after dep done = %+v, want Passed", res)
	}
}

// TestRunInvalidOverrideBlocks: a run: label that the model is not
// pinned to → Blocked with the placement reason, before any work.
func TestRunInvalidOverrideBlocks(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b", "run:igpu")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("Result = %+v, want Blocked", res)
	}
	if got := board.items["card1"].State; got != "s-blocked" {
		t.Errorf("card state = %s, want s-blocked", got)
	}
	if harness.writes != 0 {
		t.Errorf("harness ran while placement was blocked, want 0")
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	joined := ""
	for _, c := range comments {
		joined += c.CommentHTML
	}
	if !strings.Contains(joined, "can't work") {
		t.Errorf("comments missing the placement reason:\n%s", joined)
	}
}

// TestRunCloudWaits: cloud placement is not wired until C3 — the card
// stays Ready, one comment, no harness.
func TestRunCloudWaits(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "cloud-model"),
	})
	harness := &fakeHarness{passAfter: 1, good: "ok", bad: "x", file: "health.py"}
	r := testRunner(t, board, harness, contentChecker{file: "health.py", want: "ok"})

	res, err := runJob(t, r, "card1", "epic1", "model:cloud-model")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Waiting {
		t.Fatalf("Result = %+v, want Waiting", res)
	}
	if got := board.items["card1"].State; got != "s-ready" {
		t.Errorf("card state = %s, want s-ready", got)
	}
	n1, _ := board.ListComments(context.Background(), "proj1", "card1")
	if len(n1) != 1 || !strings.Contains(n1[0].CommentHTML, "cloud execution is not wired yet") {
		t.Fatalf("comments = %+v, want one cloud-waiting comment", n1)
	}
	if res, err = runJob(t, r, "card1", "epic1", "model:cloud-model"); err != nil || !res.Waiting {
		t.Fatalf("second Run = %+v, %v", res, err)
	}
	n2, _ := board.ListComments(context.Background(), "proj1", "card1")
	if len(n2) != 1 {
		t.Errorf("second run posted a duplicate comment")
	}
}

// TestRunRejectsNonCard: a card without the plan-task marker is blocked,
// not guessed at.
func TestRunRejectsNonCard(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: "<p>some free text, not a card</p>",
	})
	r := testRunner(t, board, &fakeHarness{passAfter: 1}, contentChecker{})

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("Result = %+v, want Blocked", res)
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	joined := ""
	for _, c := range comments {
		joined += c.CommentHTML
	}
	if !strings.Contains(joined, "not a task card") {
		t.Errorf("comments missing the reason:\n%s", joined)
	}
}

// TestJobParse: the payload contract — run_task accepted, other
// triggers and missing fields rejected.
func TestJobParse(t *testing.T) {
	good := `{"trigger":"run_task","board":{"project_id":"p1"},"item":{"id":"i1","parent":"e1"}}`
	j, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse(good): %v", err)
	}
	if j.Item.Parent != "e1" || j.Board.ProjectID != "p1" {
		t.Errorf("parsed = %+v", j)
	}
	for name, raw := range map[string]string{
		"plan":     `{"trigger":"plan_epic","board":{"project_id":"p"},"item":{"id":"i","parent":"e"}}`,
		"noparent": `{"trigger":"run_task","board":{"project_id":"p"},"item":{"id":"i"}}`,
		"noboard":  `{"trigger":"run_task","item":{"id":"i","parent":"e"}}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) = nil error, want one", name)
		}
	}
}

// capabilityCardHTML is a card whose executor is a capability worker (plan
// §3.7): a Capability: paragraph, no harness work. The acceptance check is
// present because ParseTaskCard requires one, but the runner never runs it
// on a capability card.
func capabilityCardHTML(id, what, cap string) string {
	base := cardHTML(id, what, "", "", "true", "S", "local-27b")
	return strings.Replace(base,
		"<p><strong>Acceptance check:",
		"<p><strong>Capability:</strong> "+cap+"</p>\n<p><strong>Acceptance check:", 1)
}

// fakeCaps is a scripted CapabilityClient for the no-LLM-round path: Run
// records what it was asked (capability, corr, input) and returns the
// scripted Result or error.
type fakeCaps struct {
	mu        sync.Mutex
	calls     int
	lastCap   string
	lastCorr  string
	lastInput map[string]any
	result    queue.Result
	err       error
}

func (f *fakeCaps) Run(ctx context.Context, capability, corr string, input map[string]any) (queue.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastCap = capability
	f.lastCorr = corr
	f.lastInput = input
	return f.result, f.err
}

func (f *fakeCaps) snapshot() (int, string, string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastCap, f.lastCorr, f.lastInput
}

// newCapabilityRunner builds a Runner wired to the scripted capability
// client. The capability path never touches the harness, the check, or the
// git repo, so those are placeholders.
func newCapabilityRunner(t *testing.T, board *fakeBoard, caps CapabilityClient) *Runner {
	t.Helper()
	return &Runner{
		Board:       board,
		Registry:    testRegistry(),
		Repo:        t.TempDir(),
		Harness:     &fakeHarness{passAfter: 1, good: "ok", file: "x"},
		Checks:      contentChecker{file: "x", want: "ok"},
		Live:        testLive,
		MaxAttempts: 3,
		Caps:        caps,
	}
}

// TestRunCapabilityPasses: a Ready card carrying a capability: runs end to
// end with NO LLM round — it is submitted to cap.<cap>, the ok Result lands
// on the card as a comment, and the card advances to Done.
func TestRunCapabilityPasses(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "describe the photos", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "describe the batch", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: capabilityCardHTML("describe", "describe the batch of photos", "image.describe"),
	})
	caps := &fakeCaps{result: queue.Result{
		CorrelationID: "card1", Status: queue.ResultOK, Summary: "3 photos described",
		Artifacts: []string{"/artifacts/image.describe/card1/captions.json"}, Worker: "image.describe@gpu",
	}}
	r := newCapabilityRunner(t, board, caps)

	res, err := runJob(t, r, "card1", "epic1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed || res.Waiting || res.Blocked {
		t.Fatalf("Result = %+v, want Passed", res)
	}
	// The capability was submitted once, to the right queue, with the card
	// id as the correlation id and the description as the input.
	if calls, capName, corr, input := caps.snapshot(); calls != 1 || capName != "image.describe" || corr != "card1" {
		t.Errorf("caps called %d times, cap=%q corr=%q, want 1/image.describe/card1", calls, capName, corr)
	} else if input["prompt"] != "describe the batch of photos" {
		t.Errorf("input prompt = %q, want the card description", input["prompt"])
	}
	// No LLM round: the harness never ran.
	if _, ok := r.Harness.(*fakeHarness); !ok {
		t.Fatalf("harness type = %T", r.Harness)
	}
	if n := r.Harness.(*fakeHarness).writes; n != 0 {
		t.Errorf("harness ran %d times on a capability card, want 0", n)
	}
	// The card advanced to Done and carries both the submit and the result.
	if got := board.items["card1"].State; got != "s-done" {
		t.Errorf("card state = %s, want s-done", got)
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	var joined string
	for _, c := range comments {
		joined += c.CommentHTML + "\n"
	}
	for _, want := range []string{"submitted to cap.image.describe", "capability image.describe — ok", "captions.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("comments missing %q:\n%s", want, joined)
		}
	}
}

// TestRunCapabilityNonOKBlocks: an error, timeout, or cancelled Result is
// not a pass — the card is Blocked with the reason (the run did not crash:
// the card state is settled, so the bridge does not re-trigger it).
func TestRunCapabilityNonOKBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result queue.Result
		want   string
	}{
		{"error", queue.Result{Status: queue.ResultError, Error: "boom"}, "capability image.describe ended error: boom"},
		{"timeout", queue.Result{Status: queue.ResultTimeout, Error: "no Result on flow.card1 before the wait deadline"}, "capability image.describe ended timeout"},
		{"cancelled", queue.Result{Status: queue.ResultCancelled, Error: "the worker was stopped"}, "capability image.describe ended cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := newFakeBoard()
			board.add(plane.WorkItem{ID: "epic1", Name: "describe the photos", State: "s-ready", SequenceID: 2})
			board.add(plane.WorkItem{
				ID: "card1", Name: "describe the batch", State: "s-ready", Parent: "epic1", SequenceID: 3,
				DescriptionHTML: capabilityCardHTML("describe", "describe the batch of photos", "image.describe"),
			})
			caps := &fakeCaps{result: tc.result}
			r := newCapabilityRunner(t, board, caps)

			res, err := runJob(t, r, "card1", "epic1")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.Blocked {
				t.Fatalf("Result = %+v, want Blocked", res)
			}
			if got := board.items["card1"].State; got != "s-blocked" {
				t.Errorf("card state = %s, want s-blocked", got)
			}
			if n := r.Harness.(*fakeHarness).writes; n != 0 {
				t.Errorf("harness ran %d times on a capability card, want 0", n)
			}
			comments, _ := board.ListComments(context.Background(), "proj1", "card1")
			var joined string
			for _, c := range comments {
				joined += c.CommentHTML + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("comments missing %q:\n%s", tc.want, joined)
			}
		})
	}
}

// TestRunCapabilityNoClientBlocks: a capability card on a Runner with no
// capability client wired (broker not configured) is Blocked, not run. A
// normal card on the same runner is unaffected.
func TestRunCapabilityNoClientBlocks(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "describe the photos", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "describe the batch", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: capabilityCardHTML("describe", "describe the batch of photos", "image.describe"),
	})
	r := newCapabilityRunner(t, board, nil) // Caps is nil

	res, err := runJob(t, r, "card1", "epic1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("Result = %+v, want Blocked", res)
	}
	if got := board.items["card1"].State; got != "s-blocked" {
		t.Errorf("card state = %s, want s-blocked", got)
	}
	comments, _ := board.ListComments(context.Background(), "proj1", "card1")
	var joined string
	for _, c := range comments {
		joined += c.CommentHTML + "\n"
	}
	if !strings.Contains(joined, "no capability client is wired") {
		t.Errorf("comments missing the no-client reason:\n%s", joined)
	}
}

// TestRunCapabilityInvalidNameBlocks: a card whose Capability: value is not
// a valid capability name is Blocked before anything is submitted.
func TestRunCapabilityInvalidNameBlocks(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "describe the photos", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "describe the batch", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: capabilityCardHTML("describe", "describe the batch of photos", "Bad-Name"),
	})
	caps := &fakeCaps{result: queue.Result{Status: queue.ResultOK}}
	r := newCapabilityRunner(t, board, caps)

	res, err := runJob(t, r, "card1", "epic1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Blocked {
		t.Fatalf("Result = %+v, want Blocked", res)
	}
	if calls, _, _, _ := caps.snapshot(); calls != 0 {
		t.Errorf("capability was submitted %d times on an invalid name, want 0", calls)
	}
	if got := board.items["card1"].State; got != "s-blocked" {
		t.Errorf("card state = %s, want s-blocked", got)
	}
}

// TestRunNormalCardUnaffected: a card with no capability: still runs an LLM
// round (the harness runs, the check gates the pass) — the capability path
// must not have swallowed the normal path.
func TestRunNormalCardUnaffected(t *testing.T) {
	board := newFakeBoard()
	board.add(plane.WorkItem{ID: "epic1", Name: "Add a /health endpoint", State: "s-ready", SequenceID: 2})
	board.add(plane.WorkItem{
		ID: "card1", Name: "health endpoint", State: "s-ready", Parent: "epic1", SequenceID: 3,
		DescriptionHTML: cardHTML("health", "add a /health endpoint", "health.py", "", "test -f health.py", "S", "local-27b"),
	})
	harness := &fakeHarness{passAfter: 1, good: "print('ok')", bad: "bad", file: "health.py"}
	r := &Runner{
		Board: board, Registry: testRegistry(), Repo: newRepo(t),
		Harness: harness, Checks: contentChecker{file: "health.py", want: "ok"},
		Live: testLive, MaxAttempts: 3,
		Caps: &fakeCaps{result: queue.Result{Status: queue.ResultOK}}, // wired, but must be ignored
	}

	res, err := runJob(t, r, "card1", "epic1", "model:local-27b")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed {
		t.Fatalf("Result = %+v, want Passed", res)
	}
	if harness.writes != 1 {
		t.Errorf("harness ran %d times on a normal card, want 1", harness.writes)
	}
	if calls, _, _, _ := r.Caps.(*fakeCaps).snapshot(); calls != 0 {
		t.Errorf("capability was submitted %d times for a normal card, want 0", calls)
	}
	if got := board.items["card1"].State; got != "s-done" {
		t.Errorf("card state = %s, want s-done", got)
	}
}
