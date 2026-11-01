// Command plane-migrate-projects splits the monolithic AAB project into
// one project per epic (the board standard — see docs/work-board/NOTES.md).
//
// The source of truth is the AAB project itself (the old kanban board is
// retired and stale). Every AAB item that carries a board-migrate marker
//
//	migrated-from: old-board epic=<uuid>
//	migrated-from: old-board task=<uuid> epic=<uuid>
//
// is RECREATED (Plane has no cross-project move) in its epic's new
// project, tagged with
//
//	split-from: AAB-<seq>
//
// so a re-run is idempotent: items whose twin already exists are
// skipped. After the create phase, the old AAB items are hard-deleted —
// but only the marker-tagged ones that have a twin in the target, so
// AAB items without a marker (or with none yet) are never touched.
//
// The epic → project mapping is hardcoded (the old-board epic UUID →
// new project identifier); the project NAME is the epic item's name on
// AAB, verbatim. Every new project gets the standard board (8 states +
// the epic label) and all workspace members as editors, via
// internal/planeepic — the same scaffold `plane-epic create` uses.
//
// Run it: -dry-run first, then for real (create + verify), then a
// second pass without -no-delete for the AAB clean-up.
//
// Environment (process env, falling back to ~/.config/jarvis/agent.env):
//
//	PLANE_URL, PLANE_API_KEY, PLANE_WORKSPACE  (as for every Plane tool)
//
// Flags:
//
//	-source    source project id or identifier (default $PLANE_PROJECT, "AAB")
//	-dry-run   print the plan; make no Plane writes
//	-no-delete skip the AAB clean-up phase (verify still requires twins)
//	-delay     pause between writes (default 250ms — Plane rate limits)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planeepic"
)

// epicIdentifiers maps the old-board epic UUID (from the AAB marker) to
// the new project's identifier. Project name = the epic item's AAB name.
var epicIdentifiers = map[string]string{
	"95a3bc1c-1980-469e-9ace-15d8682dfd57": "E2E", // Full e2e test and fix of voice chat. (AAB-1)
	"01753016-3335-4af1-bce1-4b7dd8c29320": "TST", // Test (AAB-2)
	"329068b7-0b48-403b-a520-86136a10f836": "NBR", // NoBrakes (AAB-3)
	"f4cf6031-19b0-4985-9444-a363dde6d905": "MOE", // vram cache benchmarking of MoE models (AAB-4)
	"933d6d1f-58ae-4627-bd1f-b9b44093fc04": "QDB", // Make new interactive queue dashboard (AAB-5)
	"ed9ecd12-6d2d-4342-92df-93f46312d8d2": "VCH", // Own voice channel: camera mics + direct Sonos/Google speakers (no Home Assistant) (AAB-6)
	"e991aacc-3c48-4c1d-a137-a2e4f348f16e": "T2I", // T2I image generator benchmark (AAB-7)
	"a09c0197-4b00-4241-9a16-6bd110fd6df7": "WBD", // Work board: Plane + Plan with model (AAB-8)
}

var (
	epicMarkerRE = regexp.MustCompile(`migrated-from: old-board epic=([0-9a-fA-F-]{36})`)
	taskMarkerRE = regexp.MustCompile(`migrated-from: old-board task=([0-9a-fA-F-]{36}) epic=([0-9a-fA-F-]{36})`)
)

// srcItem is a source item with its marker classification.
type srcItem struct {
	item     *plane.WorkItem
	kind     string // "epic" or "task"
	epicUUID string // old-board epic the item belongs to
}

type options struct {
	source   string
	dryRun   bool
	noDelete bool
	delay    time.Duration
}

func main() {
	if err := run(optionsFromArgs()); err != nil {
		fmt.Fprintln(os.Stderr, "plane-migrate-projects:", err)
		os.Exit(1)
	}
}

func optionsFromArgs() options {
	o := options{}
	flag.StringVar(&o.source, "source", "", "source project id or identifier (default $PLANE_PROJECT, AAB)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print the plan; make no Plane writes")
	flag.BoolVar(&o.noDelete, "no-delete", false, "skip the AAB clean-up phase")
	flag.DurationVar(&o.delay, "delay", 250*time.Millisecond, "pause between writes")
	flag.Parse()
	if o.source == "" {
		o.source = envOrEnvFile("PLANE_PROJECT", "AAB")
	}
	return o
}

func run(o options) error {
	c, err := planeClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	src, err := resolveProject(ctx, c, o.source)
	if err != nil {
		return err
	}
	fmt.Printf("source project: %s (%s)\n", src.Name, src.Identifier)

	items, err := c.ListWorkItems(ctx, src.ID)
	if err != nil {
		return fmt.Errorf("listing source items: %w", err)
	}
	srcLabels, err := c.ListLabels(ctx, src.ID)
	if err != nil {
		return fmt.Errorf("listing source labels: %w", err)
	}
	srcStates, err := c.ListStates(ctx, src.ID)
	if err != nil {
		return fmt.Errorf("listing source states: %w", err)
	}
	srcLabelName := map[string]string{} // label id -> name
	for _, l := range srcLabels {
		srcLabelName[l.ID] = l.Name
	}
	srcStateName := map[string]string{} // state id -> name
	for _, s := range srcStates {
		srcStateName[s.ID] = s.Name
	}

	// Classify the source items.
	var marked []srcItem
	var untouched int
	for i := range items {
		it := &items[i]
		if m := epicMarkerRE.FindStringSubmatch(it.DescriptionHTML); m != nil {
			marked = append(marked, srcItem{it, "epic", m[1]})
			continue
		}
		if m := taskMarkerRE.FindStringSubmatch(it.DescriptionHTML); m != nil {
			marked = append(marked, srcItem{it, "task", m[2]})
			continue
		}
		untouched++
	}
	// epics first (tasks point at the new epic items)
	sort.SliceStable(marked, func(i, j int) bool {
		if marked[i].kind != marked[j].kind {
			return marked[i].kind == "epic"
		}
		return marked[i].item.SequenceID < marked[j].item.SequenceID
	})
	fmt.Printf("items: %d total, %d marker-tagged (to migrate), %d untouched (stay in %s)\n",
		len(items), len(marked), untouched, src.Identifier)
	if o.dryRun {
		fmt.Println("(dry run — no writes)")
	}

	// Scaffold every target project (idempotent).
	targets := map[string]*target{} // old-board epic uuid -> target
	for _, si := range marked {
		if _, ok := targets[si.epicUUID]; ok {
			continue
		}
		id, ok := epicIdentifiers[strings.ToLower(si.epicUUID)]
		if !ok {
			return fmt.Errorf("no identifier for epic %s (item %s) — extend epicIdentifiers", si.epicUUID, si.item.Name)
		}
		name := si.item.Name
		if si.kind != "epic" {
			// first sighting via a task: the epic's name is its own AAB item
			var found bool
			for _, other := range marked {
				if other.kind == "epic" && other.epicUUID == si.epicUUID {
					name = other.item.Name
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("epic %s has tasks but no epic item in %s", si.epicUUID, src.Identifier)
			}
		}
		t, err := scaffold(ctx, c, name, id, o.dryRun)
		if err != nil {
			return err
		}
		targets[si.epicUUID] = t
	}

	// Create phase.
	newEpicID := map[string]string{} // old-board epic uuid -> new epic item id
	created, skipped := 0, 0
	for _, si := range marked {
		t := targets[si.epicUUID]
		marker := fmt.Sprintf("split-from: %s-%d", src.Identifier, si.item.SequenceID)
		twin, ok := t.twin(marker)
		if ok {
			skipped++
			fmt.Printf("  = %s-%d %q: twin already in %s\n", src.Identifier, si.item.SequenceID, si.item.Name, t.project.Identifier)
			if si.kind == "epic" {
				newEpicID[si.epicUUID] = twin.ID
			}
			continue
		}
		if o.dryRun {
			fmt.Printf("  + would create %s-%d %q in %s (%s, parent=%s)\n",
				src.Identifier, si.item.SequenceID, si.item.Name, t.project.Identifier,
				srcStateName[si.item.State], parentRef(si, t, newEpicID))
			continue
		}
		wi, err := createItem(ctx, c, t, si, marker, srcLabelName, srcStateName, newEpicID, o.delay)
		if err != nil {
			return fmt.Errorf("creating %s-%d %q: %w", src.Identifier, si.item.SequenceID, si.item.Name, err)
		}
		created++
		if si.kind == "epic" {
			newEpicID[si.epicUUID] = wi.ID
		}
		t.known = append(t.known, *wi)
		fmt.Printf("  + %s-%d %q -> %s-%d\n", src.Identifier, si.item.SequenceID, si.item.Name,
			t.project.Identifier, wi.SequenceID)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.delay):
		}
	}

	// Delete phase: only marker-tagged AAB items that have a twin.
	deleted := 0
	if !o.noDelete && !o.dryRun {
		for _, si := range marked {
			t := targets[si.epicUUID]
			marker := fmt.Sprintf("split-from: %s-%d", src.Identifier, si.item.SequenceID)
			if _, ok := t.twin(marker); !ok {
				return fmt.Errorf("refusing to delete %s-%d %q: no twin in %s",
					src.Identifier, si.item.SequenceID, si.item.Name, t.project.Identifier)
			}
			err := c.DeleteWorkItem(ctx, src.ID, si.item.ID)
			gone := false
			if err != nil {
				var apiErr *plane.APIError
				// Plane cascade-deletes children when a parent work item
				// is deleted — a 404 here means the item is already gone
				// (its parent epic went first), which is the goal.
				if !errors.As(err, &apiErr) || !apiErr.IsStatus(404) {
					return fmt.Errorf("deleting %s-%d: %w", src.Identifier, si.item.SequenceID, err)
				}
				gone = true
			}
			if gone {
				fmt.Printf("  - %s-%d %q already gone (cascade-deleted with parent)\n", src.Identifier, si.item.SequenceID, si.item.Name)
			} else {
				deleted++
				fmt.Printf("  - deleted %s-%d %q from %s\n", src.Identifier, si.item.SequenceID, si.item.Name, src.Identifier)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(o.delay):
			}
		}
	} else if !o.dryRun {
		fmt.Println("delete phase skipped (-no-delete)")
	}

	if o.dryRun {
		fmt.Println("(dry run — nothing written, verification skipped)")
	} else if err := verify(ctx, c, src, marked, targets, o); err != nil {
		return err
	}
	fmt.Printf("done: %d created, %d skipped (twins), %d deleted, %d untouched\n", created, skipped, deleted, untouched)
	return nil
}

// target is one new per-epic project with its cached metadata.
type target struct {
	project *plane.Project
	states  map[string]string // state name (lower) -> id
	labels  map[string]string // label name (lower) -> id
	members map[string]bool   // target project member user ids
	known   []plane.WorkItem  // items known to live here (created or twin)
}

func (t *target) twin(marker string) (*plane.WorkItem, bool) {
	for i := range t.known {
		if strings.Contains(t.known[i].DescriptionHTML, marker) {
			return &t.known[i], true
		}
	}
	return nil, false
}

// scaffold finds or creates the target project and loads its metadata.
// Under dryRun it only reads: an existing project is inspected, a
// missing one is reported as "would create".
func scaffold(ctx context.Context, c *plane.Client, name, identifier string, dryRun bool) (*target, error) {
	var p *plane.Project
	if dryRun {
		// Plane sanitizes nothing: show the name EnsureProject would create.
		name = planeepic.SanitizeProjectName(name)
		p = findProject(ctx, c, name, identifier)
		if p == nil {
			fmt.Printf("project %s: would create %q\n", identifier, name)
			return &target{project: &plane.Project{Name: name, Identifier: identifier}}, nil
		}
		fmt.Printf("project %s: exists (%s id=%s)\n", identifier, p.Name, p.ID)
	} else {
		var err error
		p, err = planeepic.EnsureProject(ctx, c, name, identifier)
		if err != nil {
			return nil, err
		}
		if err := planeepic.EnsureBoard(ctx, c, p.ID); err != nil {
			return nil, fmt.Errorf("project %s: %w", identifier, err)
		}
		ws, err := c.ListWorkspaceMembers(ctx)
		if err != nil {
			return nil, err
		}
		var emails []string
		for _, m := range ws {
			emails = append(emails, m.Email)
		}
		if _, _, err := planeepic.EnsureMembers(ctx, c, p.ID, emails); err != nil {
			return nil, fmt.Errorf("project %s: %w", identifier, err)
		}
	}
	states, err := c.ListStates(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	labelList, err := c.ListLabels(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	members, err := c.ListProjectMembers(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	// Existing items (none expected on a fresh project, but a re-run or a
	// hand-made project may have some — the twin check must see them).
	existing, err := c.ListWorkItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	fmt.Printf("project %s: %s (id=%s), %d states, %d labels, %d members, %d existing items\n",
		identifier, p.Name, p.ID, len(states), len(labelList), len(members), len(existing))
	return &target{
		project: p,
		states:  stateMap(states),
		labels:  labelMap(labelList),
		members: memberSet(members),
		known:   existing,
	}, nil
}

func stateMap(list []plane.State) map[string]string {
	out := make(map[string]string, len(list))
	for _, s := range list {
		out[strings.ToLower(s.Name)] = s.ID
	}
	return out
}

func labelMap(list []plane.Label) map[string]string {
	out := make(map[string]string, len(list))
	for _, l := range list {
		out[strings.ToLower(l.Name)] = l.ID
	}
	return out
}

func memberSet(list []plane.ProjectMember) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, m := range list {
		out[m.ID] = true
	}
	return out
}

// createItem recreates one source item in the target project.
func createItem(ctx context.Context, c *plane.Client, t *target, si srcItem, marker string,
	srcLabelName, srcStateName map[string]string, newEpicID map[string]string,
	delay time.Duration) (*plane.WorkItem, error) {
	src := si.item
	desc := src.DescriptionHTML
	if strings.TrimSpace(desc) == "" {
		desc = "<p>" + marker + "</p>"
	} else {
		desc = desc + "<p>" + marker + "</p>"
	}
	stateName := srcStateName[src.State]
	wi := plane.WorkItemCreate{
		Name:            src.Name,
		DescriptionHTML: desc,
		State:           t.states[strings.ToLower(stateName)],
		Priority:        src.Priority,
		Labels:          mapLabels(ctx, c, t, src, srcLabelName, delay),
	}
	if wi.State == "" {
		return nil, fmt.Errorf("state %q has no match in %s", stateName, t.project.Identifier)
	}
	for _, a := range src.Assignees {
		if t.members[a] {
			wi.Assignees = append(wi.Assignees, a)
		}
	}
	if si.kind == "task" {
		wi.Parent = newEpicID[si.epicUUID]
	}
	created, err := c.CreateWorkItem(ctx, t.project.ID, wi)
	if err != nil {
		return nil, fmt.Errorf("creating %q: %w", wi.Name, err)
	}
	return created, nil
}

// mapLabels maps the source item's labels into the target by name,
// creating missing labels in the target on demand.
func mapLabels(ctx context.Context, c *plane.Client, t *target, src *plane.WorkItem,
	srcLabelName map[string]string, delay time.Duration) []string {
	out := make([]string, 0, len(src.Labels))
	for _, id := range src.Labels {
		name, ok := srcLabelName[id]
		if !ok {
			continue
		}
		if tid, ok := t.labels[strings.ToLower(name)]; ok {
			out = append(out, tid)
			continue
		}
		l, err := c.CreateLabel(ctx, t.project.ID, name, "#6366F1")
		if err != nil {
			continue
		}
		t.labels[strings.ToLower(name)] = l.ID
		out = append(out, l.ID)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	return out
}

// verify recounts the board and fails on any mismatch: no marker-tagged
// source item may remain without its twin (or at all, once deleted).
func verify(ctx context.Context, c *plane.Client, src *plane.Project, marked []srcItem, targets map[string]*target, o options) error {
	fmt.Println("verifying...")
	remaining, err := c.ListWorkItems(ctx, src.ID)
	if err != nil {
		return err
	}
	stillMarked := 0
	for i := range remaining {
		desc := remaining[i].DescriptionHTML
		if epicMarkerRE.MatchString(desc) || taskMarkerRE.MatchString(desc) {
			stillMarked++
			if !o.noDelete {
				return fmt.Errorf("%s-%d %q still in the source project after migration",
					src.Identifier, remaining[i].SequenceID, remaining[i].Name)
			}
		}
	}
	if o.noDelete {
		fmt.Printf("source %s: %d marker-tagged item(s) remain (delete phase skipped); every twin was checked in the delete gate\n",
			src.Identifier, stillMarked)
	}
	for uuid, t := range targets {
		want := 0
		for _, si := range marked {
			if si.epicUUID == uuid {
				want++
			}
		}
		got := len(t.known)
		if got < want {
			return fmt.Errorf("project %s has %d items, want %d", t.project.Identifier, got, want)
		}
		if got > want {
			fmt.Printf("note: project %s has %d items, %d migrated (the rest pre-existed)\n",
				t.project.Identifier, got, want)
		} else {
			fmt.Printf("project %s: %d item(s) ok\n", t.project.Identifier, got)
		}
	}
	return nil
}

func parentRef(si srcItem, t *target, newEpicID map[string]string) string {
	if si.kind != "task" {
		return "none"
	}
	if id, ok := newEpicID[si.epicUUID]; ok {
		return id
	}
	return "(new epic item)"
}

// --- env helpers (same convention as board-migrate) --------------------------------

func planeClient() (*plane.Client, error) {
	cfg := plane.Config{
		BaseURL:   envOrEnvFile("PLANE_URL", ""),
		APIKey:    envOrEnvFile("PLANE_API_KEY", ""),
		Workspace: envOrEnvFile("PLANE_WORKSPACE", "jarvis"),
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("PLANE_URL is not set (process env or ~/.config/jarvis/agent.env)")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("PLANE_API_KEY is not set (process env or ~/.config/jarvis/agent.env)")
	}
	return plane.New(cfg)
}

func resolveProject(ctx context.Context, c *plane.Client, ref string) (*plane.Project, error) {
	p := findProject(ctx, c, ref, ref) // id, name, or identifier
	if p == nil {
		return nil, fmt.Errorf("project %q not found in workspace", ref)
	}
	return p, nil
}

// findProject looks a project up by id, name or identifier
// (name/identifier case-insensitive); nil when not found.
func findProject(ctx context.Context, c *plane.Client, name, identifier string) *plane.Project {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil
	}
	for i := range projects {
		if (name != "" && (projects[i].ID == name || strings.EqualFold(projects[i].Name, name))) ||
			(identifier != "" && strings.EqualFold(projects[i].Identifier, identifier)) {
			return &projects[i]
		}
	}
	return nil
}

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
		if ok && strings.TrimSpace(k) == key {
			return v
		}
	}
	return def
}
