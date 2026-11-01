// Command board-migrate migrates open items from the old homemade kanban
// board into the Plane work board (P5 of docs/work-board/PLAN.md), then the
// old board is retired.
//
// Mapping (the old board has no epic status, so every epic counts as open):
//
//   - every epic            -> a Plane work item in Backlog with the `epic`
//     label (title + description carried over)
//   - every open task       -> a sub-item under its migrated epic
//     (status != "done"; done tasks are skipped)
//   - old status -> new state: backlog -> Backlog, in-progress/review ->
//     In Progress ("review" is the old board's done-awaiting-close state,
//     and the new board's "Plan review" column is for plan approval, not
//     code review)
//
// Each migrated item carries a marker line in its description:
//
//	migrated-from: old-board epic=<uuid>
//	migrated-from: old-board task=<uuid> epic=<uuid>
//
// which makes the migration idempotent: a re-run lists the project, matches
// markers, and skips what already exists. A final recount compares Plane
// against the old board and exits non-zero on any mismatch.
//
// Environment (process env, falling back to ~/.config/jarvis/agent.env):
//
//	PLANE_URL        Plane base URL (required)
//	PLANE_API_KEY    v1 API key (required)
//	PLANE_WORKSPACE  workspace slug (default "jarvis")
//	PLANE_PROJECT    project id or identifier (default "AAB")
//
// Flags:
//
//	-board-dir    old board dir (default $JARVIS_DATA_DIR, else ~/.local/share/jarvis)
//	-dry-run     print what would be created; make no Plane calls
//	-verify-only  no creation; just recount Plane and compare with the board
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

var (
	epicMarkerRE = regexp.MustCompile(`migrated-from: old-board epic=([0-9a-fA-F-]{36})`)
	taskMarkerRE = regexp.MustCompile(`migrated-from: old-board task=([0-9a-fA-F-]{36})`)
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "board-migrate")
	boardDir := flag.String("board-dir", "", "old board dir (default $JARVIS_DATA_DIR or ~/.local/share/jarvis)")
	dryRun := flag.Bool("dry-run", false, "print the migration plan and exit; no Plane calls")
	verifyOnly := flag.Bool("verify-only", false, "recount Plane and compare with the old board; no creation")
	flag.Parse()
	if err := run(log, *boardDir, *dryRun, *verifyOnly); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, boardDir string, dryRun, verifyOnly bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if boardDir == "" {
		boardDir = os.Getenv("JARVIS_DATA_DIR")
	}
	if boardDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/home/agent"
		}
		boardDir = filepath.Join(home, ".local", "share", "jarvis")
	}

	board, err := loadBoard(boardDir)
	if err != nil {
		return fmt.Errorf("load board: %w", err)
	}
	log.Info("old board", "dir", boardDir, "epics", len(board.Epics),
		"openTasks", len(board.Tasks), "totalTasks", board.TotalTasks)

	if dryRun {
		printPlan(board)
		return nil
	}

	c, err := planeClient()
	if err != nil {
		return err
	}
	projectID, err := resolveProject(ctx, c)
	if err != nil {
		return err
	}
	states, err := stateIDs(ctx, c, projectID)
	if err != nil {
		return err
	}
	epicLabel, err := c.LabelByName(ctx, projectID, "epic")
	if err != nil {
		return fmt.Errorf("epic label: %w", err)
	}

	if !verifyOnly {
		created, skipped, err := migrate(ctx, c, projectID, states, epicLabel.ID, board)
		if err != nil {
			return err
		}
		log.Info("migration done", "created", created, "skippedExisting", skipped)
	}

	// Acceptance check: open item counts match between the old board and
	// Plane. Recount from the live project, not from the creation loop.
	items, err := c.ListWorkItems(ctx, projectID)
	if err != nil {
		return fmt.Errorf("recount: %w", err)
	}
	haveEpics, haveTasks := indexItems(items)
	missingEpics := missing(boardEpicIDs(board.Epics), haveEpics)
	missingTasks := missing(boardTaskIDs(board.Tasks), haveTasks)
	fmt.Printf("plane project %s: %d/%d migrated epics, %d/%d migrated open tasks\n",
		projectID, len(haveEpics), len(board.Epics), len(haveTasks), len(board.Tasks))
	if len(missingEpics) > 0 || len(missingTasks) > 0 {
		return fmt.Errorf("count mismatch; missing epics %v, missing tasks %v",
			missingEpics, missingTasks)
	}
	fmt.Printf("MATCH: all %d epics and %d open tasks from the old board are on Plane\n",
		len(board.Epics), len(board.Tasks))
	return nil
}

// board is the old-board view the migration works from: all epics and the
// open tasks (status != done) in the old board's order.
type board struct {
	Epics      []kanban.Epic
	Tasks      []kanban.Task
	TotalTasks int
}

func loadBoard(dir string) (board, error) {
	store, err := kanban.Open(dir)
	if err != nil {
		return board{}, err
	}
	epics, err := store.ListEpics()
	if err != nil {
		return board{}, err
	}
	tasks, err := store.ListTasks()
	if err != nil {
		return board{}, err
	}
	open := make([]kanban.Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Status != kanban.StatusDone {
			open = append(open, t)
		}
	}
	sortBoard(epics, open)
	return board{Epics: epics, Tasks: open, TotalTasks: len(tasks)}, nil
}

// sortBoard keeps the old board's ordering: epics by creation time, tasks in
// their epic's task_ids order (dependency order), then by creation time for
// tasks no epic lists.
func sortBoard(epics []kanban.Epic, tasks []kanban.Task) {
	sort.Slice(epics, func(i, j int) bool {
		if epics[i].CreatedAt.Equal(epics[j].CreatedAt) {
			return epics[i].ID < epics[j].ID
		}
		return epics[i].CreatedAt.Before(epics[j].CreatedAt)
	})
	posOf, epicIdx := map[string]int{}, map[string]int{}
	for i, e := range epics {
		epicIdx[e.ID] = i
		for p, tid := range e.TaskIDs {
			posOf[e.ID+"/"+tid] = p
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		ea, eb := epicIdx[a.EpicID], epicIdx[b.EpicID]
		if ea != eb {
			return ea < eb
		}
		pa, pb := posOf[a.EpicID+"/"+a.ID], posOf[b.EpicID+"/"+b.ID]
		if pa != pb {
			return pa < pb
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
}

// stateNameFor maps an old-board status to a Plane state name.
func stateNameFor(s kanban.Status) string {
	switch s {
	case kanban.StatusInProgress, kanban.StatusReview:
		return "In Progress"
	default:
		return "Backlog"
	}
}

// epicDescription carries the old description plus the idempotency marker.
func epicDescription(e kanban.Epic) string {
	var b strings.Builder
	if d := strings.TrimSpace(e.Description); d != "" {
		b.WriteString("<p>" + escape(d) + "</p>")
	}
	b.WriteString("<p><em>Migrated from the old homemade board (P5).</em></p>")
	b.WriteString("<p>migrated-from: old-board epic=" + e.ID + "</p>")
	return b.String()
}

// taskDescription carries the old content plus status metadata and the
// idempotency marker.
func taskDescription(t kanban.Task) string {
	var b strings.Builder
	if c := strings.TrimSpace(t.Content); c != "" {
		b.WriteString("<p>" + escape(c) + "</p>")
	}
	b.WriteString("<p><em>Migrated from the old board (P5).</em></p>")
	b.WriteString("<p>migrated-from: old-board task=" + t.ID + " epic=" + t.EpicID + "</p>")
	meta := "old status: " + string(t.Status) + "; priority: " + strconv.Itoa(t.Priority)
	if t.Model != "" {
		meta += "; old model: " + t.Model
	}
	b.WriteString("<p>" + escape(meta) + "</p>")
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// migrate creates any items whose marker is not already on the project.
// Returns (created, skipped) counts.
func migrate(ctx context.Context, c *plane.Client, projectID string, states map[string]string, epicLabelID string, b board) (created, skipped int, err error) {
	items, err := c.ListWorkItems(ctx, projectID)
	if err != nil {
		return 0, 0, fmt.Errorf("list existing: %w", err)
	}
	haveEpics, haveTasks := indexItems(items)

	epicPlaneID := map[string]string{}
	for _, e := range b.Epics {
		if id, ok := haveEpics[e.ID]; ok {
			epicPlaneID[e.ID] = id
			skipped++
			continue
		}
		wi, err := c.CreateWorkItem(ctx, projectID, plane.WorkItemCreate{
			Name:            e.Title,
			DescriptionHTML: epicDescription(e),
			State:           states["Backlog"],
			Labels:          []string{epicLabelID},
		})
		if err != nil {
			return created, skipped, fmt.Errorf("create epic %q: %w", e.Title, err)
		}
		epicPlaneID[e.ID] = wi.ID
		created++
		fmt.Printf("created epic  %-50s -> %s\n", truncate(e.Title, 50), wi.ID)
	}

	for _, t := range b.Tasks {
		if _, ok := haveTasks[t.ID]; ok {
			skipped++
			continue
		}
		parent, ok := epicPlaneID[t.EpicID]
		if !ok {
			// Defensive: every open task on the live board has an epic;
			// if not, land it top-level rather than dropping it.
			fmt.Printf("WARNING: task %s (%s) has no migrated epic; creating top-level\n", t.ID, t.Title)
		}
		wi, err := c.CreateWorkItem(ctx, projectID, plane.WorkItemCreate{
			Name:            t.Title,
			DescriptionHTML: taskDescription(t),
			Parent:          parent,
			State:           states[stateNameFor(t.Status)],
		})
		if err != nil {
			return created, skipped, fmt.Errorf("create task %q: %w", t.Title, err)
		}
		created++
		fmt.Printf("created task  %-50s -> %s (parent %s)\n", truncate(t.Title, 50), wi.ID, parent)
	}
	return created, skipped, nil
}

// indexItems maps old-board IDs to Plane item IDs via the description
// markers. Returns (epic old-ID -> plane ID, task old-ID -> plane ID).
func indexItems(items []plane.WorkItem) (map[string]string, map[string]string) {
	epics, tasks := map[string]string{}, map[string]string{}
	for _, it := range items {
		if m := epicMarkerRE.FindStringSubmatch(it.DescriptionHTML); m != nil {
			epics[m[1]] = it.ID
		}
		if m := taskMarkerRE.FindStringSubmatch(it.DescriptionHTML); m != nil {
			tasks[m[1]] = it.ID
		}
	}
	return epics, tasks
}

func missing(want []string, have map[string]string) []string {
	var out []string
	for _, id := range want {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func boardEpicIDs(epics []kanban.Epic) []string {
	ids := make([]string, len(epics))
	for i, e := range epics {
		ids[i] = e.ID
	}
	return ids
}

func boardTaskIDs(tasks []kanban.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

func printPlan(b board) {
	fmt.Printf("dry run: would create %d epics and %d sub-items on Plane\n\n", len(b.Epics), len(b.Tasks))
	for _, e := range b.Epics {
		n := 0
		for _, t := range b.Tasks {
			if t.EpicID == e.ID {
				n++
			}
		}
		fmt.Printf("epic   %-52s %s (%d open tasks)\n", truncate(e.Title, 52), e.ID, n)
		for _, t := range b.Tasks {
			if t.EpicID != e.ID {
				continue
			}
			fmt.Printf("  task  %-52s %s [%s -> %s]\n", truncate(t.Title, 52), t.ID, t.Status, stateNameFor(t.Status))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func stateIDs(ctx context.Context, c *plane.Client, projectID string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range []string{"Backlog", "In Progress"} {
		st, err := c.StateByName(ctx, projectID, n)
		if err != nil {
			return nil, fmt.Errorf("state %q: %w", n, err)
		}
		out[n] = st.ID
	}
	return out, nil
}

func resolveProject(ctx context.Context, c *plane.Client) (string, error) {
	want := envOrEnvFile("PLANE_PROJECT", "AAB")
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.ID == want || p.Identifier == want {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("project %q not found in workspace", want)
}

func planeClient() (*plane.Client, error) {
	cfg := plane.Config{
		BaseURL:   envOrEnvFile("PLANE_URL", ""),
		APIKey:    envOrEnvFile("PLANE_API_KEY", ""),
		Workspace: envOrEnvFile("PLANE_WORKSPACE", "jarvis"),
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("PLANE_URL is not set (process env or ~/.config/jarvis/agent.env)")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("PLANE_API_KEY is not set (process env or ~/.config/jarvis/agent.env)")
	}
	return plane.New(cfg)
}

// envOrEnvFile reads key from the process environment, falling back to the
// daemon env file ~/.config/jarvis/agent.env (same pattern as the P4 live
// test), then to def.
func envOrEnvFile(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	path := os.Getenv("PLANE_ENV_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return def
		}
		path = filepath.Join(home, ".config", "jarvis", "agent.env")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return def
}
