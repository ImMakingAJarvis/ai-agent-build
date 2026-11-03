package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// TestLiveB5ApprovalAndReplan is the B5 acceptance against the real board:
// a toy epic in a scratch project gets a scripted plan delivered, then
//
//   - moving the epic to Plan approved moves its sub-items to Ready and
//     queues a run_task job for each (the approval path);
//   - commenting on the epic and moving it back to Planning queues a
//     re-plan job whose instructions carry the comment — the bot's own
//     comments filtered out (the re-plan path);
//   - delivering the re-plan over the old one updates the page, refreshes
//     the kept cards in place and deletes the dropped card.
//
// The bridge is driven through CheckItem (the same trigger evaluation the
// webhooks and the catch-up run), and its plan queue is a fake — the test
// never touches the real RabbitMQ. The scratch project is deleted
// afterwards. Credentials come from the environment (or
// ~/.config/jarvis/agent.env via the env helpers); without them the test
// skips, so `go test ./...` stays green off this machine.
func TestLiveB5ApprovalAndReplan(t *testing.T) {
	planeURL := liveGet("PLANE_URL")
	botKey := liveGet("PLANE_API_KEY")
	adminKey := liveGet("PLANE_ADMIN_API_KEY")
	botEmail := liveGet("PLANE_BOT_EMAIL")
	botPassword := liveGet("PLANE_BOT_PASSWORD")
	if planeURL == "" || botKey == "" || botEmail == "" || botPassword == "" {
		t.Skip("PLANE_URL/PLANE_API_KEY/PLANE_BOT_EMAIL/PLANE_BOT_PASSWORD unavailable")
	}
	defer plane.AcquireLiveLock(t, 8*time.Minute)()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	bot, err := plane.New(plane.Config{
		BaseURL: planeURL, APIKey: botKey, Workspace: "jarvis",
		Email: botEmail, Password: botPassword,
	})
	if err != nil {
		t.Fatalf("plane.New(bot): %v", err)
	}

	// --- scratch project with the states and labels the paths key on ---
	proj, err := bot.CreateProject(ctx, "b5_test_"+time.Now().Format("20060102_150405"), "B5"+rand3(t))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Logf("scratch project %s (%s)", proj.Identifier, proj.ID)
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if err := bot.DeleteProject(dctx, proj.ID); err != nil {
			t.Errorf("cleanup: delete scratch project %s: %v", proj.ID, err)
		}
	})
	states := map[string]*plane.State{}
	for _, s := range []struct {
		name, group, color string
		seq                float64
	}{
		{"Planning", "unstarted", "#94A3B8", 98304},
		{planReviewState, "unstarted", "#FBBF24", 131072},
		{readyState, "unstarted", "#34D399", 196608},
		{"Plan approved", "started", "#60A5FA", 262144},
	} {
		st, err := bot.CreateState(ctx, proj.ID, s.name, s.group, s.color, s.seq)
		if err != nil {
			t.Fatalf("CreateState %q: %v", s.name, err)
		}
		states[s.name] = st
	}
	epicLbl, err := bot.CreateLabel(ctx, proj.ID, "epic", "#111111")
	if err != nil {
		t.Fatalf("CreateLabel epic: %v", err)
	}
	planLbl, err := bot.CreateLabel(ctx, proj.ID, "plan:qwen3.8-27b", "#222222")
	if err != nil {
		t.Fatalf("CreateLabel plan:qwen3.8-27b: %v", err)
	}
	epic, err := bot.CreateWorkItem(ctx, proj.ID, plane.WorkItemCreate{
		Name:            "B5 live: toy epic",
		DescriptionHTML: "<p>Build a toy.</p>",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem epic: %v", err)
	}
	if _, err := bot.AddLabels(ctx, proj.ID, epic.ID, epicLbl.ID, planLbl.ID); err != nil {
		t.Fatalf("AddLabels epic: %v", err)
	}

	// The admin account posts the "human" comment; project membership is
	// per-project, so add it to the scratch project as editor (role 20) —
	// a plain add grants the default viewer role, which cannot comment.
	if adminKey != "" {
		adminEmail := liveGet("PLANE_ADMIN_EMAIL")
		if adminEmail == "" {
			t.Fatalf("PLANE_ADMIN_API_KEY set but PLANE_ADMIN_EMAIL missing")
		}
		adminID := ""
		members, err := bot.ListWorkspaceMembers(ctx)
		if err != nil {
			t.Fatalf("ListWorkspaceMembers: %v", err)
		}
		for i := range members {
			if strings.EqualFold(members[i].Email, adminEmail) {
				adminID = members[i].ID
			}
		}
		if adminID == "" {
			t.Fatalf("admin %s is not a workspace member", adminEmail)
		}
		if err := v1Post(ctx, planeURL, botKey,
			fmt.Sprintf("/api/v1/workspaces/jarvis/projects/%s/members/", proj.ID),
			map[string]any{"member": adminID, "role": 20}); err != nil {
			t.Fatalf("add admin to scratch project: %v", err)
		}
	}

	// --- the bridge under test: real board, real store, fake plan queue ---
	work := t.TempDir()
	secretFile := filepath.Join(work, "secret")
	if err := os.WriteFile(secretFile, []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := agentjob.Open(filepath.Join(work, "jobs"))
	if err != nil {
		t.Fatalf("agentjob.Open: %v", err)
	}
	fq := &FakePlanQueue{Behind: 0}
	b, err := New(Config{
		Plane:        bot,
		Store:        store,
		Projects:     []string{proj.ID},
		ProjectNames: map[string]string{proj.ID: proj.Identifier},
		WebhookURL:   "http://127.0.0.1:0/webhook/plane",
		SecretFile:   secretFile,
		BotEmail:     botEmail,
		BoardURL:     planeURL,
		Workspace:    "jarvis",
		PlanQueue:    fq,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// --- deliver a scripted first plan ---
	reg := &registry.File{Models: []registry.Model{{
		ID: "qwen3.8-27b", Roles: []string{"executor"}, Enabled: true,
	}}}
	d := &planworker.Deliverer{Plane: bot, Registry: reg}
	liveJob := func() *planworker.Job {
		return &planworker.Job{
			Trigger: planworker.TriggerPlan,
			Board:   planworker.Board{Workspace: "jarvis", Project: proj.Identifier, ProjectID: proj.ID},
			Item:    planworker.Item{ID: epic.ID, Name: epic.Name, Description: "Build a toy.", SequenceID: epic.SequenceID},
			ModelID: "qwen3.8-27b",
			Effort:  "high",
		}
	}
	res1 := &planworker.Result{
		Plan: "# B5 live toy plan\n\nGoal: build a toy. First plan.",
		Tasks: planworker.TaskList{Epic: epic.ID, Tasks: []planworker.Task{
			{ID: "t1", Title: "Base", Description: "Build the base", AcceptanceCheck: "go test ./...", Size: "S", ExecutorModel: "qwen3.8-27b"},
			{ID: "t2", Title: "Top", Description: "Build the top", Dependencies: []string{"t1"}, AcceptanceCheck: "go test ./...", Size: "M", ExecutorModel: "qwen3.8-27b"},
		}},
		Model: registry.Model{ID: "qwen3.8-27b"},
	}
	if _, err := d.Deliver(ctx, liveJob(), res1); err != nil {
		t.Fatalf("Deliver first plan: %v", err)
	}
	items, err := bot.ListWorkItems(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	byTask := map[string]*plane.WorkItem{}
	for i := range items {
		it := &items[i]
		if it.Parent != epic.ID {
			continue
		}
		if tid, ok := planeMarkerValue(t, it.DescriptionHTML); ok {
			byTask[tid] = it
		}
	}
	if len(byTask) != 2 {
		t.Fatalf("sub-items = %d, want 2 (t1, t2)", len(byTask))
	}
	t.Log("first plan delivered: epic and t1/t2 in Plan review")

	// --- path 1: approve ---
	if _, err := bot.SetState(ctx, proj.ID, epic.ID, states["Plan approved"].ID); err != nil {
		t.Fatalf("move epic to Plan approved: %v", err)
	}
	n, err := b.CheckItem(ctx, proj.ID, epic.ID)
	if err != nil {
		t.Fatalf("CheckItem (approval): %v", err)
	}
	if n != 2 {
		t.Fatalf("approval CheckItem queued %d jobs, want 2 (one run_task per sub-item)", n)
	}
	for tid, it := range byTask {
		got, err := bot.GetWorkItem(ctx, proj.ID, it.ID)
		if err != nil {
			t.Fatalf("GetWorkItem %s: %v", tid, err)
		}
		if got.State != states[readyState].ID {
			t.Errorf("task %s: state = %s, want Ready (%s)", tid, got.State, states[readyState].ID)
		}
	}
	for tid, it := range byTask {
		if _, err := store.Get("plane-" + TriggerRun + "-" + it.ID); err != nil {
			t.Errorf("run_task job for %s missing: %v", tid, err)
		}
	}
	if pubs := fq.published(); len(pubs) != 0 {
		t.Errorf("approval published %d queue messages, want 0 (run_task jobs land in the store only)", len(pubs))
	}
	t.Log("approval: t1/t2 moved to Ready with run_task jobs queued")

	// --- path 2: comment and re-plan ---
	humanComment := "Split task two into two tasks."
	if adminKey != "" {
		admin, err := plane.New(plane.Config{BaseURL: planeURL, APIKey: adminKey, Workspace: "jarvis"})
		if err != nil {
			t.Fatalf("plane.New(admin): %v", err)
		}
		if _, err := admin.AddComment(ctx, proj.ID, epic.ID, "<p>"+humanComment+"</p>"); err != nil {
			t.Fatalf("admin comment: %v", err)
		}
	} else {
		t.Log("PLANE_ADMIN_API_KEY unset: no human comment posted; the re-plan must queue with empty comments")
	}
	if _, err := bot.SetState(ctx, proj.ID, epic.ID, states["Planning"].ID); err != nil {
		t.Fatalf("move epic back to Planning: %v", err)
	}
	n, err = b.CheckItem(ctx, proj.ID, epic.ID)
	if err != nil {
		t.Fatalf("CheckItem (re-plan): %v", err)
	}
	if n != 1 {
		t.Fatalf("re-plan CheckItem queued %d jobs, want 1", n)
	}
	replanID := "plane-" + TriggerReplan + "-" + epic.ID
	job, err := store.Get(replanID)
	if err != nil {
		t.Fatalf("re-plan job not queued: %v", err)
	}
	var inst map[string]any
	if err := json.Unmarshal([]byte(job.Instructions), &inst); err != nil {
		t.Fatalf("re-plan instructions not JSON: %v", err)
	}
	if inst["trigger"] != TriggerReplan || inst["model_id"] != "qwen3.8-27b" {
		t.Errorf("re-plan instructions trigger/model = %v/%v, want %s/qwen3.8-27b",
			inst["trigger"], inst["model_id"], TriggerReplan)
	}
	comments, _ := inst["comments"].([]any)
	if adminKey != "" {
		found := false
		for _, c := range comments {
			if strings.Contains(strings.TrimSpace(c.(string)), humanComment) {
				found = true
			}
		}
		if !found {
			t.Errorf("re-plan comments = %v, want to include the human comment %q", comments, humanComment)
		}
	}
	for _, c := range comments { // the bot's machine notes are filtered out
		if s, _ := c.(string); strings.Contains(s, "Plan ready") {
			t.Errorf("re-plan comments carry the bot's summary: %v", comments)
		}
	}
	pubs := fq.published()
	if len(pubs) != 1 {
		t.Fatalf("queue publishes = %d, want 1 (the re-plan)", len(pubs))
	}
	if pubs[0].ID != replanID || pubs[0].Priority != queue.PlanEpicPriority || pubs[0].Kind != queue.KindAgent {
		t.Errorf("published id/priority/kind = %s/%d/%s, want %s/%d/%s",
			pubs[0].ID, pubs[0].Priority, pubs[0].Kind, replanID, queue.PlanEpicPriority, queue.KindAgent)
	}
	t.Logf("re-plan queued: %s (%d human comment(s))", replanID, len(comments))

	// --- the re-plan delivers over the first: page updated, t2 (now in
	// Ready, dropped by the new plan) deleted, t1 refreshed, t3 new ---
	job2 := liveJob()
	job2.Trigger = planworker.TriggerReplan
	job2.Comments = []string{humanComment}
	res2 := &planworker.Result{
		Plan: "# B5 live toy plan (re-plan)\n\nGoal: build a toy. Re-planned.",
		Tasks: planworker.TaskList{Epic: epic.ID, Tasks: []planworker.Task{
			{ID: "t1", Title: "Base", Description: "Build the base (revised)", AcceptanceCheck: "go test ./...", Size: "S", ExecutorModel: "qwen3.8-27b"},
			{ID: "t3", Title: "Top one", Description: "Build the top, part one", Dependencies: []string{"t1"}, AcceptanceCheck: "go test ./...", Size: "S", ExecutorModel: "qwen3.8-27b"},
		}},
		Model: registry.Model{ID: "qwen3.8-27b"},
	}
	t2ID := byTask["t2"].ID
	if _, err := d.Deliver(ctx, job2, res2); err != nil {
		t.Fatalf("Deliver re-plan: %v", err)
	}
	pages, err := bot.ListPages(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1 (the re-plan reuses the page)", len(pages))
	}
	body, err := bot.GetPage(ctx, proj.ID, pages[0].ID)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if !strings.Contains(body.DescriptionHTML, "Re-planned.") || strings.Contains(body.DescriptionHTML, "First plan.") {
		t.Errorf("page body not replaced by the re-plan: %q", body.DescriptionHTML)
	}
	items2, err := bot.ListWorkItems(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListWorkItems (2nd): %v", err)
	}
	gotT2, err := bot.GetWorkItem(ctx, proj.ID, t2ID)
	if err == nil && gotT2.State != "" {
		t.Errorf("dropped card t2 (Ready) still exists in state %s after the re-plan", gotT2.State)
	}
	byTask2 := map[string]*plane.WorkItem{}
	for i := range items2 {
		it := &items2[i]
		if it.Parent != epic.ID {
			continue
		}
		if tid, ok := planeMarkerValue(t, it.DescriptionHTML); ok {
			byTask2[tid] = it
		}
	}
	if _, ok := byTask2["t2"]; ok {
		t.Errorf("dropped card t2 still listed after the re-plan")
	}
	for tid, want := range map[string]string{"t1": "Build the base (revised)", "t3": "Build the top, part one"} {
		it, ok := byTask2[tid]
		if !ok {
			t.Errorf("task %s missing after the re-plan", tid)
			continue
		}
		if !strings.Contains(it.DescriptionHTML, want) {
			t.Errorf("task %s description %q does not carry %q", tid, it.DescriptionHTML, want)
		}
		if it.State != states[planReviewState].ID {
			t.Errorf("task %s state = %s, want Plan review (%s)", tid, it.State, states[planReviewState].ID)
		}
	}
	epic2, err := bot.GetWorkItem(ctx, proj.ID, epic.ID)
	if err != nil {
		t.Fatalf("GetWorkItem epic (2nd): %v", err)
	}
	if epic2.State != states[planReviewState].ID {
		t.Errorf("epic state = %s, want Plan review (%s)", epic2.State, states[planReviewState].ID)
	}
	if n := strings.Count(epic2.DescriptionHTML, plane.PlanPageMarker); n != 1 {
		t.Errorf("epic description has %d plan-page lines after the re-plan, want 1: %q", n, epic2.DescriptionHTML)
	}
	t.Log("re-plan delivered: page updated, t2 deleted, t1 refreshed, t3 new")
}

// planeMarkerValue is the deliverer's plan-task: marker lookup, reached
// through the public plane package so the bridge tests do not need
// planworker's unexported helper.
func planeMarkerValue(t *testing.T, html string) (string, bool) {
	t.Helper()
	const marker = plane.PlanTaskMarker
	i := strings.Index(html, marker)
	if i < 0 {
		return "", false
	}
	j := i + len(marker)
	for j < len(html) && (html[j] == '-' || (html[j] >= 'a' && html[j] <= 'z') || (html[j] >= '0' && html[j] <= '9')) {
		j++
	}
	if j == i+len(marker) {
		return "", false
	}
	return html[i+len(marker) : j], true
}
