// Package planeepic scaffolds a new "one project per epic" board (the
// board standard — see docs/work-board/NOTES.md): a Plane project with
// the standard state set, the epic label, and the workspace's members
// added as editors so they can move items.
package planeepic

import (
	"context"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// stateSpec is one column of the standard board.
type stateSpec struct {
	name     string
	group    string
	color    string
	sequence float64
}

// StandardStates is the state set every epic project gets — the same
// columns as AAB (scripts/plane-setup.sh). Sequences are (i+1)*1000 so
// new states can be inserted later without renumbering.
var StandardStates = []stateSpec{
	{"Backlog", "backlog", "#94A3B8", 1000},
	{"Planning", "unstarted", "#0EA5E9", 2000},
	{"Plan review", "unstarted", "#6366F1", 3000},
	{"Plan approved", "unstarted", "#8B5CF6", 4000},
	{"Ready", "unstarted", "#22C55E", 5000},
	{"In progress", "started", "#F59E0B", 6000},
	{"Blocked", "started", "#EF4444", 7000},
	{"Done", "completed", "#10B981", 8000},
}

// EpicLabelName and EpicLabelColor are the label that marks an epic item
// (the plan_epic trigger looks for it).
const (
	EpicLabelName  = "epic"
	EpicLabelColor = "#6366F1"
)

// EditorRole is the Plane project role code for editor — viewer (the
// default on a bare add) cannot move work items.
const EditorRole = 20

// SanitizeProjectName makes a title usable as a Plane project name. This
// Plane build rejects a long list of "special characters" in project
// names ("." ":" "+" "(" ")" "&" "-" "," "@" ...), so anything outside
// the known-safe set (letters, digits, space, /, _, ') becomes a space.
// Work-item names have no such restriction — only project names.
func SanitizeProjectName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '_' || r == '\'':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// EnsureProject finds the project by name or identifier
// (case-insensitive) or creates it with the given identifier. Plane
// requires the identifier at creation — it is not auto-generated. The
// name is sanitized (Plane rejects special characters in project names);
// work items created elsewhere keep the unsanitized title.
func EnsureProject(ctx context.Context, c *plane.Client, name, identifier string) (*plane.Project, error) {
	name = SanitizeProjectName(name)
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	for i := range projects {
		if strings.EqualFold(projects[i].Name, name) || strings.EqualFold(projects[i].Identifier, identifier) {
			return &projects[i], nil
		}
	}
	p, err := c.CreateProject(ctx, name, identifier)
	if err != nil {
		return nil, fmt.Errorf("creating project %q (%s): %w", name, identifier, err)
	}
	return p, nil
}

// EnsureBoard gives the project the standard board: the standard states
// and the epic label. Idempotent — existing states and labels (matched
// by name, case-insensitively) are left alone. It also prunes the extra
// default states Plane auto-creates on new projects (Todo, Cancelled,
// ...), but only when no work item references one — same rule as
// scripts/plane-setup.sh.
func EnsureBoard(ctx context.Context, c *plane.Client, projectID string) error {
	states, err := c.ListStates(ctx, projectID)
	if err != nil {
		return fmt.Errorf("listing states: %w", err)
	}
	have := make(map[string]bool, len(states))
	for _, s := range states {
		have[strings.ToLower(s.Name)] = true
	}
	for _, spec := range StandardStates {
		if have[strings.ToLower(spec.name)] {
			continue
		}
		if _, err := c.CreateState(ctx, projectID, spec.name, spec.group, spec.color, spec.sequence); err != nil {
			return fmt.Errorf("creating state %q: %w", spec.name, err)
		}
	}
	// Prune non-standard states Plane auto-creates, once the project has
	// states of its own. Re-list after creating: the new states are not in
	// the original listing.
	states, err = c.ListStates(ctx, projectID)
	if err != nil {
		return fmt.Errorf("listing states: %w", err)
	}
	used, err := usedStateIDs(ctx, c, projectID)
	if err != nil {
		return err
	}
	for _, s := range states {
		if isStandard(s.Name) || used[s.ID] {
			continue
		}
		if err := c.DeleteState(ctx, projectID, s.ID); err != nil {
			return fmt.Errorf("deleting unused state %q: %w", s.Name, err)
		}
	}
	labels, err := c.ListLabels(ctx, projectID)
	if err != nil {
		return fmt.Errorf("listing labels: %w", err)
	}
	found := false
	for _, l := range labels {
		if strings.EqualFold(l.Name, EpicLabelName) {
			found = true
			break
		}
	}
	if !found {
		if _, err := c.CreateLabel(ctx, projectID, EpicLabelName, EpicLabelColor); err != nil {
			return fmt.Errorf("creating %q label: %w", EpicLabelName, err)
		}
	}
	return nil
}

// isStandard reports whether name is one of the standard board states
// (case-insensitive).
func isStandard(name string) bool {
	for _, spec := range StandardStates {
		if strings.EqualFold(spec.name, name) {
			return true
		}
	}
	return false
}

// usedStateIDs returns the set of state IDs referenced by work items in
// the project — a state in use must not be deleted.
func usedStateIDs(ctx context.Context, c *plane.Client, projectID string) (map[string]bool, error) {
	items, err := c.ListWorkItems(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing work items: %w", err)
	}
	used := make(map[string]bool, len(items))
	for _, it := range items {
		used[it.State] = true // WorkItem.State is the state ID
	}
	return used, nil
}

// EnsureMembers adds the given workspace-member emails to the project as
// editors. Idempotent: members already in the project are skipped (the
// member list carries no role, so membership is the only check).
// Emails that are not workspace members are skipped with a note — the
// caller decides what to do about them.
func EnsureMembers(ctx context.Context, c *plane.Client, projectID string, emails []string) (added, skipped []string, err error) {
	wsMembers, err := c.ListWorkspaceMembers(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing workspace members: %w", err)
	}
	byEmail := make(map[string]string, len(wsMembers))
	for _, m := range wsMembers {
		byEmail[strings.ToLower(m.Email)] = m.ID
	}
	projMembers, err := c.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("listing project members: %w", err)
	}
	inProject := make(map[string]bool, len(projMembers))
	for _, m := range projMembers {
		inProject[m.ID] = true
	}
	for _, email := range emails {
		userID, ok := byEmail[strings.ToLower(strings.TrimSpace(email))]
		if !ok {
			skipped = append(skipped, email)
			continue
		}
		if inProject[userID] {
			continue
		}
		if err := c.AddProjectMember(ctx, projectID, userID, EditorRole); err != nil {
			return added, skipped, fmt.Errorf("adding %s as editor: %w", email, err)
		}
		added = append(added, email)
	}
	return added, skipped, nil
}
