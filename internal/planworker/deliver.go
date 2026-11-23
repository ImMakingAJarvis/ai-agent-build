package planworker

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planeassign"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// PlanReviewState is the state for planned-but-unapproved work
// (docs/work-board/PLAN.md, "Approval in Plane"): tasks and their epic sit
// in it until Nathan approves (B5) or re-plans (B5).
const PlanReviewState = "Plan review"

// ReadyState is the state planned tasks are created in. Ready is the
// run trigger, so the work is queued as soon as the plan lands.
const ReadyState = "Ready"

// DoneState is where the planning card goes once its tasks are queued.
const DoneState = "Done"

// The marker convention (P5/P6) lives in the plane package — the bridge
// matches on the same strings when it recognises an epic as planned.
const (
	planTaskMarker = plane.PlanTaskMarker
	planPageMarker = plane.PlanPageMarker
)

// Deliverer delivers a finished plan to the Plane board (B4,
// docs/work-board/PLAN.md, "Approval in Plane"):
//
//   - the plan document becomes a project page, linked from the epic
//     (the plan-page: line in the epic's description) and summarised in a
//     comment on it;
//   - the tasks become sub-items of the planning card in Ready, each
//     assigned to the Qwen worker (jarvis-bot2), with a blocked-by
//     relation per dependency and the model:<id> label of that executor;
//   - the planning card itself moves to Done.
//
// Delivery converges: a re-run against the same epic reuses the sub-items
// and page it already created (found via their markers) and replaces the
// link line rather than stacking another, so a partial failure followed by
// a retry does not duplicate cards.
//
// A re-plan (replan_epic, B5) delivers a new plan over the old one: the
// page body is replaced, the tasks the new plan still has are refreshed
// in place, and the cards of tasks it dropped are deleted (only while
// they are still in Plan review or Ready).
type Deliverer struct {
	// Plane is the board client (needs the bot session for pages).
	Plane *plane.Client
	// Registry is the loaded model registry: a task's executor model must
	// be an enabled executor for delivery to accept it — placement.Sync
	// owns which model: labels the board keeps and would delete the rest.
	Registry *registry.File
}

// Delivery is what a successful delivery left on the board.
type Delivery struct {
	// Page is the plan page (new or reused).
	Page *plane.Page
	// Items maps each task id to the work-item id it landed on.
	Items map[string]string
}

// Deliver writes res's plan and tasks to the board for job's epic. Any
// failure returns an error and the epic is left where it was: only a fully
// successful delivery moves it to Done.
func (d *Deliverer) Deliver(ctx context.Context, job *Job, res *Result) (*Delivery, error) {
	if job.Board.ProjectID == "" {
		return nil, fmt.Errorf("plan job %s: no board project id", msgID(job))
	}
	if len(res.Tasks.Tasks) == 0 {
		return nil, fmt.Errorf("plan job %s: plan has no tasks to deliver", msgID(job))
	}
	projectID := job.Board.ProjectID
	epicID := job.Item.ID

	state, err := d.Plane.StateByName(ctx, projectID, ReadyState)
	if err != nil {
		return nil, fmt.Errorf("state %q: %w", ReadyState, err)
	}
	done, err := d.Plane.StateByName(ctx, projectID, DoneState)
	if err != nil {
		return nil, fmt.Errorf("state %q: %w", DoneState, err)
	}
	qwenID, err := d.qwenMember(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := d.modelLabels(ctx, projectID, res.Tasks)
	if err != nil {
		return nil, err
	}
	items, err := d.subItems(ctx, job, res, projectID, epicID, state, labels, qwenID)
	if err != nil {
		return nil, err
	}
	page, err := d.planPage(ctx, projectID, job, res.Plan)
	if err != nil {
		return nil, err
	}
	if err := d.linkPage(ctx, projectID, epicID, page, res); err != nil {
		return nil, err
	}
	if err := d.summaryComment(ctx, projectID, epicID, job, res, page); err != nil {
		return nil, err
	}
	if _, err := d.Plane.SetState(ctx, projectID, epicID, done.ID); err != nil {
		return nil, fmt.Errorf("move planning card to %s: %w", DoneState, err)
	}
	return &Delivery{Page: page, Items: items}, nil
}

// qwenMember is the workspace member id of the Qwen worker (jarvis-bot2).
func (d *Deliverer) qwenMember(ctx context.Context) (string, error) {
	members, err := d.Plane.ListWorkspaceMembers(ctx)
	if err != nil {
		return "", fmt.Errorf("list workspace members: %w", err)
	}
	for _, m := range members {
		if strings.EqualFold(m.Email, planeassign.QwenEmail) {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("workspace has no member %s", planeassign.QwenEmail)
}

// modelLabels resolves the model:<id> label id for every executor model
// the plan chose. A label the project does not have yet is created in
// placement's model colour (a fresh project, before placement.Sync has
// run — it will keep the label). A model that is not an enabled executor
// in the registry is rejected: placement.Sync would delete the label
// again and the C1 placement cannot route a job to it.
func (d *Deliverer) modelLabels(ctx context.Context, projectID string, tl TaskList) (map[string]string, error) {
	existing, err := d.Plane.ListLabels(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	byName := map[string]string{}
	for _, l := range existing {
		byName[strings.ToLower(l.Name)] = l.ID
	}
	out := map[string]string{}
	for _, t := range tl.Tasks {
		if _, ok := out[t.ExecutorModel]; ok {
			continue
		}
		if !d.enabledExecutor(t.ExecutorModel) {
			return nil, fmt.Errorf("task %q: executor model %s is not an enabled executor in the registry", t.ID, t.ExecutorModel)
		}
		name := placement.ModelLabel(t.ExecutorModel)
		if id, ok := byName[strings.ToLower(name)]; ok {
			out[t.ExecutorModel] = id
			continue
		}
		created, err := d.Plane.CreateLabel(ctx, projectID, name, placement.ModelColor)
		if err != nil {
			return nil, fmt.Errorf("create model label %s: %w", name, err)
		}
		out[t.ExecutorModel] = created.ID
	}
	return out, nil
}

// enabledExecutor reports whether id is an enabled model with the executor
// role in d.Registry.
func (d *Deliverer) enabledExecutor(id string) bool {
	if d.Registry == nil {
		return false
	}
	for i := range d.Registry.Models {
		m := d.Registry.Models[i]
		if m.ID != id || !m.Enabled {
			continue
		}
		for _, r := range m.Roles {
			if r == "executor" {
				return true
			}
		}
	}
	return false
}

// subItems creates (or reuses, via the plan-task: marker) the planning
// card's task sub-items in Ready, assigns the Qwen worker, and wires
// their blocked-by relations.
func (d *Deliverer) subItems(ctx context.Context, job *Job, res *Result, projectID, epicID string, state *plane.State, labels map[string]string, qwenID string) (map[string]string, error) {
	// Cards a previous (partially failed) delivery already created.
	have := map[string]string{} // task id -> work item id
	items, err := d.Plane.ListWorkItems(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	for i := range items {
		it := &items[i]
		if it.Parent != epicID {
			continue
		}
		if tid, ok := markerValue(it.DescriptionHTML, planTaskMarker); ok {
			have[tid] = it.ID
		}
	}

	out := map[string]string{}
	for _, t := range res.Tasks.Tasks {
		if id, ok := have[t.ID]; ok {
			// Converge the leftover card: back to Plan review, with its
			// label (AddLabels merges, so the union is idempotent). On a
			// re-plan the card's content is the new plan's — refresh it.
			if job.Trigger == TriggerReplan {
				if _, err := d.Plane.SetDescriptionHTML(ctx, projectID, id, taskDescriptionHTML(t)); err != nil {
					return nil, fmt.Errorf("task %q: refresh description: %w", t.ID, err)
				}
			}
			if _, err := d.Plane.SetState(ctx, projectID, id, state.ID); err != nil {
				return nil, fmt.Errorf("task %q: reset state: %w", t.ID, err)
			}
			if _, err := d.Plane.AddLabels(ctx, projectID, id, labels[t.ExecutorModel]); err != nil {
				return nil, fmt.Errorf("task %q: label: %w", t.ID, err)
			}
			if _, err := d.Plane.SetAssignees(ctx, projectID, id, []string{qwenID}); err != nil {
				return nil, fmt.Errorf("task %q: assign qwen: %w", t.ID, err)
			}
			out[t.ID] = id
			continue
		}
		wi, err := d.Plane.CreateWorkItem(ctx, projectID, plane.WorkItemCreate{
			Name:            t.Title,
			DescriptionHTML: taskDescriptionHTML(t),
			Parent:          epicID,
			State:           state.ID,
			Labels:          []string{labels[t.ExecutorModel]},
		})
		if err != nil {
			return nil, fmt.Errorf("create sub-item for task %q: %w", t.ID, err)
		}
		if _, err := d.Plane.SetAssignees(ctx, projectID, wi.ID, []string{qwenID}); err != nil {
			return nil, fmt.Errorf("task %q: assign qwen: %w", t.ID, err)
		}
		out[t.ID] = wi.ID
	}

	// Re-plan (B5): cards of the previous plan that the new plan no longer
	// has are deleted — but only cards the delivery still owns (Plan review
	// or Ready). A card Nathan has moved elsewhere (In progress, Done, ...)
	// is left where it is: a re-plan does not delete history or in-flight
	// work.
	if job.Trigger == TriggerReplan {
		owned := map[string]bool{state.ID: true}
		if review, err := d.Plane.StateByName(ctx, projectID, PlanReviewState); err == nil {
			owned[review.ID] = true
		}
		for i := range items {
			it := &items[i]
			if it.Parent != epicID || !owned[it.State] {
				continue
			}
			tid, ok := markerValue(it.DescriptionHTML, planTaskMarker)
			if !ok || out[tid] != "" {
				continue // no marker, or the new plan still has this task
			}
			if err := d.Plane.DeleteWorkItem(ctx, projectID, it.ID); err != nil {
				return nil, fmt.Errorf("delete re-plan card %q: %w", tid, err)
			}
		}
	}

	// blocked-by: each dependency becomes a relation, added after all the
	// cards exist.
	for _, t := range res.Tasks.Tasks {
		if len(t.Dependencies) == 0 {
			continue
		}
		by := make([]string, 0, len(t.Dependencies))
		for _, dep := range t.Dependencies {
			by = append(by, out[dep])
		}
		if err := d.Plane.AddBlockedBy(ctx, projectID, out[t.ID], by...); err != nil {
			return nil, fmt.Errorf("blocked-by for task %q: %w", t.ID, err)
		}
	}
	return out, nil
}

// planPage creates the project page holding the plan document, reusing a
// page with the same name when an earlier delivery of this epic's plan
// already made one (a retry must not stack a second page). On a re-plan
// the reused page's body is the old plan — it is replaced with the new
// one (a first-plan retry leaves the body alone: same content, no churn).
func (d *Deliverer) planPage(ctx context.Context, projectID string, job *Job, plan string) (*plane.Page, error) {
	name := "Plan: " + job.Item.Name
	pages, err := d.Plane.ListPages(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list pages: %w", err)
	}
	for i := range pages {
		if pages[i].Name == name {
			if job.Trigger == TriggerReplan {
				pg, err := d.Plane.UpdatePage(ctx, projectID, pages[i].ID, plane.PageCreate{
					Name:            pages[i].Name,
					DescriptionHTML: planPageHTML(plan),
				})
				if err != nil {
					return nil, fmt.Errorf("update plan page: %w", err)
				}
				return pg, nil
			}
			return &pages[i], nil
		}
	}
	pg, err := d.Plane.CreatePage(ctx, projectID, plane.PageCreate{
		Name:            name,
		DescriptionHTML: planPageHTML(plan),
	})
	if err != nil {
		return nil, fmt.Errorf("create page: %w", err)
	}
	return pg, nil
}

// planPageHTML renders the plan document as page content: the plan is
// Markdown and the page editor takes HTML, so it goes in a <pre> block —
// Plane renders it verbatim and no conversion can mangle code or tables
// (a richer conversion is a follow-up if pages start to be edited in
// place).
func planPageHTML(plan string) string {
	return "<pre>" + html.EscapeString(plan) + "</pre>"
}

// linkPage keeps the plan-page: line in the epic's description pointing at
// the plan page — an existing line is replaced, not appended, so repeated
// deliveries do not stack links. The plan text itself lives on the page,
// so the line only carries the URL.
func (d *Deliverer) linkPage(ctx context.Context, projectID, epicID string, pg *plane.Page, res *Result) error {
	epic, err := d.Plane.GetWorkItem(ctx, projectID, epicID)
	if err != nil {
		return fmt.Errorf("get epic: %w", err)
	}
	url := d.Plane.PageLink(projectID, pg)
	desc := dropMarkerParagraph(epic.DescriptionHTML, planPageMarker)
	if desc != "" {
		desc += "\n"
	}
	desc += "<p>" + planPageMarker + url + "</p>"
	if _, err := d.Plane.SetDescriptionHTML(ctx, projectID, epicID, desc); err != nil {
		return fmt.Errorf("link plan page from epic: %w", err)
	}
	return nil
}

// summaryComment posts the model-prefixed summary on the epic (the P6
// comment convention): what was planned and where to find it.
func (d *Deliverer) summaryComment(ctx context.Context, projectID, epicID string, job *Job, res *Result, pg *plane.Page) error {
	url := d.Plane.PageLink(projectID, pg)
	reviewed := ""
	if res.Reviewed {
		reviewed = " (reviewed)"
	}
	verb := "Plan"
	if job.Trigger == TriggerReplan {
		verb = "Re-plan"
	}
	msg := fmt.Sprintf("%s: %s ready — %d task(s) in %s, assigned to %s%s. Plan: %s",
		res.Model.ID, verb, len(res.Tasks.Tasks), ReadyState, planeassign.QwenEmail, reviewed, url)
	if _, err := d.Plane.AddComment(ctx, projectID, epicID, "<p>"+htmlEscapeLine(msg)+"</p>"); err != nil {
		return fmt.Errorf("summary comment: %w", err)
	}
	return nil
}

// taskDescriptionHTML renders a task as its sub-item's description: the
// plan-task: marker line first (what the delivery and B5's re-plan use to
// find its own cards), then the task content.
func taskDescriptionHTML(t Task) string {
	var b strings.Builder
	b.WriteString("<p>" + planTaskMarker + t.ID + "</p>\n")
	b.WriteString("<p><strong>What to build:</strong> " + cardText(t.Description) + "</p>\n")
	if len(t.Files) > 0 {
		b.WriteString("<p><strong>Files:</strong> " + html.EscapeString(strings.Join(t.Files, ", ")) + "</p>\n")
	}
	if len(t.Dependencies) > 0 {
		b.WriteString("<p><strong>Blocked by:</strong> " + html.EscapeString(strings.Join(t.Dependencies, ", ")) + "</p>\n")
	}
	if t.Capability != "" {
		b.WriteString("<p><strong>Capability:</strong> " + html.EscapeString(t.Capability) + "</p>\n")
	}
	b.WriteString("<p><strong>Acceptance check:</strong> " + html.EscapeString(t.AcceptanceCheck) + "</p>\n")
	b.WriteString("<p><strong>Size:</strong> " + html.EscapeString(t.Size) + " &middot; <strong>Executor:</strong> " +
		html.EscapeString(placement.ModelLabel(t.ExecutorModel)) + "</p>")
	return b.String()
}

// cardText escapes a multi-line value for a card paragraph, keeping its
// line breaks as <br> (ParseTaskCard turns them back into newlines). The
// value is escaped first, so a literal "<br>" in it stays "&lt;br&gt;" and
// the round trip is exact.
func cardText(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>")
}

// markerValue returns the value of a "<marker><value>" occurrence in s,
// where value is a task id (schema: [a-z][a-z0-9-]*). It is HTML-agnostic:
// the markers are written as plain text inside <p> elements.
func markerValue(s, marker string) (string, bool) {
	i := strings.Index(s, marker)
	if i < 0 {
		return "", false
	}
	j := i + len(marker)
	for j < len(s) && isIDChar(s[j]) {
		j++
	}
	if j == i+len(marker) {
		return "", false
	}
	return s[i+len(marker) : j], true
}

func isIDChar(c byte) bool {
	return c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// dropMarkerParagraph removes the <p> element that contains marker (if
// any), so a re-delivery replaces its line instead of appending a second.
// A description whose marker is not inside a well-formed <p> is left as
// is: the append still happens and a human can untangle it.
func dropMarkerParagraph(desc, marker string) string {
	i := strings.Index(desc, marker)
	if i < 0 {
		return strings.TrimRight(desc, " \n")
	}
	start := strings.LastIndex(desc[:i], "<p>")
	tail := strings.Index(desc[i:], "</p>")
	if start < 0 || tail < 0 {
		return strings.TrimRight(desc, " \n")
	}
	kept := desc[:start] + desc[i+tail+len("</p>"):]
	return strings.TrimRight(kept, " \n")
}

// htmlEscapeLine HTML-escapes a single line of text for a <p> element.
func htmlEscapeLine(s string) string {
	return html.EscapeString(strings.ReplaceAll(s, "\n", "<br>"))
}

// msgID is a short job identifier for error messages.
func msgID(job *Job) string {
	if job.Item.ID != "" {
		return job.Item.ID
	}
	return job.Item.Name
}
