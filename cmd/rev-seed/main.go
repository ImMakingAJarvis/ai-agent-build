package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// One card. ParentID empty means the epic is the parent. DependsOn is
// another plan-task id. The seeder is the only creator. Re-runs match
// the plan-task marker and do not create a second card.
type card struct {
	ID, Name, What, Files, Accept, ParentID, DependsOn string
}

func cards() []card {
	return []card{
		{ID: "rev-1", Name: "1. Review config", What: "Tracker only. Mark Done after rev-1a and rev-1b are Done.", Files: "config/review.yaml, internal/reviewcfg", Accept: "go test ./internal/reviewcfg/", ParentID: "", DependsOn: ""},
		{ID: "rev-1a", Name: "1a. Write config/review.yaml", What: "Write the file in Job 1a of docs/pr-review-build-plan.md. Do not add a model to merge_allow.", Files: "config/review.yaml", Accept: "test -f config/review.yaml", ParentID: "rev-1", DependsOn: "rev-1"},
		{ID: "rev-1b", Name: "1b. Load the review config", What: "Job 1b of docs/pr-review-build-plan.md.", Files: "internal/reviewcfg", Accept: "go test ./internal/reviewcfg/", ParentID: "rev-1", DependsOn: "rev-1a"},

		{ID: "rev-2", Name: "2. Stamps and revival log", What: "Tracker only. Mark Done after rev-2a, rev-2b, and rev-2c are Done.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/", ParentID: "", DependsOn: "rev-1b"},
		{ID: "rev-2a", Name: "2a. File store for one pull request", What: "Job 2a of docs/pr-review-build-plan.md.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestStoreRoundTrip", ParentID: "rev-2", DependsOn: "rev-2"},
		{ID: "rev-2b", Name: "2b. Stamp rules", What: "Job 2b of docs/pr-review-build-plan.md. Copy the test table. Do not invent cases.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestStamp", ParentID: "rev-2", DependsOn: "rev-2a"},
		{ID: "rev-2c", Name: "2c. Revival log", What: "Job 2c of docs/pr-review-build-plan.md.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestRevival", ParentID: "rev-2", DependsOn: "rev-2b"},

		{ID: "rev-3", Name: "3. Review queues", What: "Tracker only. Mark Done after rev-3a, rev-3b, and rev-3c are Done.", Files: "rabbitmq/definitions.json, scripts/jarvis-review.sh, internal/reviewroute", Accept: "go test ./internal/reviewroute/", ParentID: "", DependsOn: "rev-2c"},
		{ID: "rev-3a", Name: "3a. Declare the four queues", What: "Job 3a of docs/pr-review-build-plan.md.", Files: "rabbitmq/definitions.json, internal/queue", Accept: "go test ./internal/queue/ -count=1", ParentID: "rev-3", DependsOn: "rev-3"},
		{ID: "rev-3b", Name: "3b. Stop using priority 100", What: "Job 3b of docs/pr-review-build-plan.md.", Files: "scripts/jarvis-review.sh", Accept: "go test ./internal/reviewroute/", ParentID: "rev-3", DependsOn: "rev-3a"},
		{ID: "rev-3c", Name: "3c. Queue name tests", What: "Job 3c of docs/pr-review-build-plan.md. Copy the test rows.", Files: "internal/reviewroute", Accept: "go test ./internal/reviewroute/ -run TestQueue", ParentID: "rev-3", DependsOn: "rev-3b"},

		{ID: "rev-4", Name: "4. Request a review, hold Fable", What: "Tracker only. Mark Done after rev-4a and rev-4b are Done.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/", ParentID: "", DependsOn: "rev-3c"},
		{ID: "rev-4a", Name: "4a. RequestReview", What: "Job 4a of docs/pr-review-build-plan.md.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/ -run TestRequest", ParentID: "rev-4", DependsOn: "rev-4"},
		{ID: "rev-4b", Name: "4b. Fable stays held", What: "Job 4b of docs/pr-review-build-plan.md.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/ -run TestFable", ParentID: "rev-4", DependsOn: "rev-4a"},

		{ID: "rev-5", Name: "5. Discord and full auto reviewer", What: "Tracker only. Mark Done after rev-5a and rev-5b are Done.", Files: "cmd/jarvis-chat", Accept: "go test ./cmd/jarvis-chat/ -count=1", ParentID: "", DependsOn: "rev-4b"},
		{ID: "rev-5a", Name: "5a. !review", What: "Job 5a of docs/pr-review-build-plan.md.", Files: "cmd/jarvis-chat/cmdparse.go", Accept: "go test ./cmd/jarvis-chat/ -run TestReviewCmd -count=1", ParentID: "rev-5", DependsOn: "rev-5"},
		{ID: "rev-5b", Name: "5b. !fullauto on names the reviewer", What: "Job 5b of docs/pr-review-build-plan.md.", Files: "cmd/jarvis-chat/fullauto.go", Accept: "go test ./cmd/jarvis-chat/ -run TestFullAuto -count=1", ParentID: "rev-5", DependsOn: "rev-5a"},

		{ID: "rev-6", Name: "6. Review worker", What: "Tracker only. Mark Done after rev-6a, rev-6b, and rev-6c are Done.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/", ParentID: "", DependsOn: "rev-5b"},
		{ID: "rev-6a", Name: "6a. Consume one queue", What: "Job 6a of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestConsume", ParentID: "rev-6", DependsOn: "rev-6"},
		{ID: "rev-6b", Name: "6b. Worktree and the Go gate", What: "Job 6b of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestGate", ParentID: "rev-6", DependsOn: "rev-6a"},
		{ID: "rev-6c", Name: "6c. Claude, and no GPU", What: "Job 6c of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestNoGPU", ParentID: "rev-6", DependsOn: "rev-6b"},

		{ID: "rev-7", Name: "7. Commits on the pull request branch", What: "Tracker only. Mark Done after rev-7a, rev-7b, and rev-7c are Done.", Files: "internal/reviewstate, internal/reviewturn", Accept: "go test ./internal/reviewturn/", ParentID: "", DependsOn: "rev-6c"},
		{ID: "rev-7a", Name: "7a. Only the token holder pushes", What: "Job 7a of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestToken", ParentID: "rev-7", DependsOn: "rev-7"},
		{ID: "rev-7b", Name: "7b. A push updates stamps", What: "Job 7b of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestPush", ParentID: "rev-7", DependsOn: "rev-7a"},
		{ID: "rev-7c", Name: "7c. Author response job", What: "Job 7c of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestAuthorJob", ParentID: "rev-7", DependsOn: "rev-7b"},

		{ID: "rev-8", Name: "8. Merge gate", What: "Tracker only. Mark Done after rev-8a and rev-8b are Done.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMerge", ParentID: "", DependsOn: "rev-7c"},
		{ID: "rev-8a", Name: "8a. Both stamps, and this turn did not push", What: "Job 8a of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMerge -count=1", ParentID: "rev-8", DependsOn: "rev-8"},
		{ID: "rev-8b", Name: "8b. Empty allow list asks Nathan", What: "Job 8b of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMergeAllow -count=1", ParentID: "rev-8", DependsOn: "rev-8a"},

		{ID: "rev-9", Name: "9. Full auto limits and the risk rating", What: "Tracker only. Mark Done after rev-9a, rev-9b, and rev-9c are Done.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/", ParentID: "", DependsOn: "rev-8b"},
		{ID: "rev-9a", Name: "9a. Two open pull requests", What: "Job 9a of docs/pr-review-build-plan.md.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestOpenCap", ParentID: "rev-9", DependsOn: "rev-9"},
		{ID: "rev-9b", Name: "9b. Ten dollar month", What: "Job 9b of docs/pr-review-build-plan.md.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestSpend", ParentID: "rev-9", DependsOn: "rev-9a"},
		{ID: "rev-9c", Name: "9c. Risk rating and the sensitive floor", What: "Job 9c of docs/pr-review-build-plan.md. Copy the path list.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestRisk", ParentID: "rev-9", DependsOn: "rev-9b"},

		{ID: "rev-10", Name: "10. Five minute check", What: "Tracker only. Mark Done after rev-10a, rev-10b, and rev-10c are Done.", Files: "internal/reviewsweep, cmd/review-sweep", Accept: "go test ./internal/reviewsweep/", ParentID: "", DependsOn: "rev-9c"},
		{ID: "rev-10a", Name: "10a. Next action table", What: "Job 10a of docs/pr-review-build-plan.md. Copy the table into tests.", Files: "internal/reviewsweep", Accept: "go test ./internal/reviewsweep/ -run TestNext", ParentID: "rev-10", DependsOn: "rev-10"},
		{ID: "rev-10b", Name: "10b. The loop", What: "Job 10b of docs/pr-review-build-plan.md.", Files: "cmd/review-sweep", Accept: "go test ./internal/reviewsweep/ -run TestSweep", ParentID: "rev-10", DependsOn: "rev-10a"},
		{ID: "rev-10c", Name: "10c. End of day", What: "Job 10c of docs/pr-review-build-plan.md.", Files: "internal/reviewsweep", Accept: "go test ./internal/reviewsweep/ -run TestDigest", ParentID: "rev-10", DependsOn: "rev-10b"},

		{ID: "rev-11", Name: "11. Plane identity", What: "Tracker only. Mark Done after rev-11a and rev-11b are Done. The Plane keys are already in the secret manager.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/", ParentID: "", DependsOn: "rev-10c"},
		{ID: "rev-11a", Name: "11a. One key per model", What: "Job 11a of docs/pr-review-build-plan.md. The keys are already stored. List names only. Never print a value.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/ -run TestKeys", ParentID: "rev-11", DependsOn: "rev-11"},
		{ID: "rev-11b", Name: "11b. Route Nathan's replies", What: "Job 11b of docs/pr-review-build-plan.md.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/ -run TestRoute", ParentID: "rev-11", DependsOn: "rev-11a"},

		{ID: "rev-12", Name: "12. Containers and the standing note", What: "Tracker only. Mark Done after rev-12a and rev-12b are Done.", Files: "docker-compose.yml, docs/agent-claude.md", Accept: "go test ./cmd/review-worker/ ./cmd/review-sweep/", ParentID: "", DependsOn: "rev-11b"},
		{ID: "rev-12a", Name: "12a. Compose services with no GPU", What: "Job 12a of docs/pr-review-build-plan.md. Do not start jarvis-engine.", Files: "docker-compose.yml", Accept: "go test ./cmd/review-worker/ -run TestNoGPU", ParentID: "rev-12", DependsOn: "rev-12"},
		{ID: "rev-12b", Name: "12b. Definition of done", What: "Job 12b of docs/pr-review-build-plan.md.", Files: "docs/agent-claude.md", Accept: "test -f docs/agent-claude.md", ParentID: "rev-12", DependsOn: "rev-12a"},
	}
}

func main() {
	c, err := plane.FromEnv()
	if err != nil {
		die(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	projects, err := c.ListProjects(ctx)
	if err != nil {
		die(err)
	}
	var project *plane.Project
	for i := range projects {
		if strings.EqualFold(projects[i].Identifier, "REV") {
			project = &projects[i]
			break
		}
	}
	if project == nil {
		die(fmt.Errorf("no Plane project REV; run plane-epic first"))
	}
	items, err := c.ListWorkItems(ctx, project.ID)
	if err != nil {
		die(err)
	}
	var epic *plane.WorkItem
	for i := range items {
		if items[i].Parent == "" && items[i].Name == "PR review queues" {
			epic = &items[i]
			break
		}
	}
	if epic == nil {
		die(fmt.Errorf("REV has no epic named %q", "PR review queues"))
	}
	backlog, err := c.StateByName(ctx, project.ID, "Backlog")
	if err != nil {
		die(err)
	}

	byMarker := map[string]plane.WorkItem{}
	for _, it := range items {
		if id, ok := marker(it.DescriptionHTML); ok {
			byMarker[id] = it
		}
	}

	created, existed := 0, 0
	for _, cd := range cards() {
		if _, ok := byMarker[cd.ID]; ok {
			existed++
			continue
		}
		parent := epic.ID
		if cd.ParentID != "" {
			p, ok := byMarker[cd.ParentID]
			if !ok {
				die(fmt.Errorf("card %s: parent %s not created yet", cd.ID, cd.ParentID))
			}
			parent = p.ID
		}
		wi, err := c.CreateWorkItem(ctx, project.ID, plane.WorkItemCreate{
			Name:            cd.Name,
			DescriptionHTML: cardHTML(cd),
			Parent:          parent,
			State:           backlog.ID,
		})
		if err != nil {
			die(fmt.Errorf("create %s: %w", cd.ID, err))
		}
		byMarker[cd.ID] = *wi
		created++
		fmt.Printf("created %s-%d %s\n", project.Identifier, wi.SequenceID, cd.ID)
		time.Sleep(4 * time.Second)
	}

	relations := 0
	for _, cd := range cards() {
		if cd.DependsOn == "" {
			continue
		}
		self := byMarker[cd.ID]
		dep := byMarker[cd.DependsOn]
		rel, err := c.Relations(ctx, project.ID, self.ID)
		if err != nil {
			die(err)
		}
		if hasBlock(rel, dep.ID) {
			continue
		}
		if err := c.AddBlockedBy(ctx, project.ID, self.ID, dep.ID); err != nil {
			die(fmt.Errorf("blocked-by %s: %w", cd.ID, err))
		}
		relations++
		time.Sleep(4 * time.Second)
	}

	// Re-list so a card created earlier in this run is checked too.
	items, err = c.ListWorkItems(ctx, project.ID)
	if err != nil {
		die(err)
	}
	badState, badLabel := 0, 0
	for _, it := range items {
		id, ok := marker(it.DescriptionHTML)
		if !ok {
			continue
		}
		if it.State != backlog.ID {
			fmt.Fprintf(os.Stderr, "bad state %s\n", id)
			badState++
		}
		for _, lid := range it.Labels {
			// Labels are IDs. Any label on a seeded card is wrong:
			// no plan: and no model:.
			fmt.Fprintf(os.Stderr, "label on %s: %s\n", id, lid)
			badLabel++
		}
	}
	fmt.Printf("created: %d existed: %d relations: %d bad-state: %d bad-label: %d\n",
		created, existed, relations, badState, badLabel)
	if badState > 0 || badLabel > 0 {
		os.Exit(1)
	}
}

func cardHTML(cd card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>plan-task: %s</p>\n", cd.ID)
	fmt.Fprintf(&b, "<p><strong>What to build:</strong> %s</p>\n", html.EscapeString(cd.What))
	fmt.Fprintf(&b, "<p><strong>Files:</strong> %s</p>\n", html.EscapeString(cd.Files))
	fmt.Fprintf(&b, "<p><strong>Acceptance check:</strong> %s</p>\n", html.EscapeString(cd.Accept))
	b.WriteString("<p><strong>Size:</strong> S · <strong>Executor:</strong> follow docs/pr-review-build-plan.md</p>")
	return b.String()
}

func marker(desc string) (string, bool) {
	const p = "plan-task: "
	i := strings.Index(desc, p)
	if i < 0 {
		return "", false
	}
	j := i + len(p)
	for j < len(desc) {
		c := desc[j]
		if c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			j++
			continue
		}
		break
	}
	if j == i+len(p) {
		return "", false
	}
	return desc[i+len(p) : j], true
}

func hasBlock(r *plane.Relations, id string) bool {
	if r == nil {
		return false
	}
	for _, b := range r.BlockedBy {
		if b.IssueID == id {
			return true
		}
	}
	return false
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "rev-seed:", err)
	os.Exit(1)
}
