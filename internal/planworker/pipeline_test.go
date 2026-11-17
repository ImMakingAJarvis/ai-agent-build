package planworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// scriptModel is a stub planning model: each Chat call returns the next
// scripted reply (or m.err, when set) and records the prompt it got, so
// tests can assert on exactly what each step was asked.
type scriptModel struct {
	id      string
	replies []string
	err     error // every call fails
	failAt  int   // only the 0-indexed call number fails (default -1)
	calls   int
	prompts []string
}

func (m *scriptModel) ID() string { return m.id }

func (m *scriptModel) Chat(ctx context.Context, msgs []models.ChatMessage) (string, error) {
	if len(msgs) == 0 {
		return "", errors.New("scriptModel: no messages")
	}
	m.prompts = append(m.prompts, msgs[0].Content)
	if m.err != nil && (m.failAt < 0 || m.calls == m.failAt) {
		m.calls++
		return "", m.err
	}
	if m.calls >= len(m.replies) {
		return "", errors.New("scriptModel: no more scripted replies")
	}
	out := m.replies[m.calls]
	m.calls++
	return out, nil
}

func (m *scriptModel) Release(ctx context.Context) error { return nil }

const (
	planDoc       = "# Plan\n\n## Goal\nbuild the thing\n"
	validTasks    = `{"epic":"ignored","tasks":[` + oneTask + `]}`
	badSizeTasks  = `{"epic":"ignored","tasks":[{"id":"a","title":"t","description":"d","acceptance_check":"x","size":"L","executor_model":"m"}]}`
	reviewedTasks = `{"epic":"ignored","tasks":[{"id":"a","title":"t (reviewed)","description":"d","acceptance_check":"go test ./...","size":"S","executor_model":"m"}]}`
)

func newTestPipeline(t *testing.T, repoRoot string) *Pipeline {
	t.Helper()
	p, err := NewPipeline(filepath.Join("..", "..", "prompts"), repoRoot, testFile(t), nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestPipelineHappyPath is the full B3 flow: the planner runs the plan and
// decompose steps, a different model runs the review, and its (valid)
// fixes replace the decomposed list.
func TestPipelineHappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("HELLO README"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "x.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	rev := &scriptModel{id: "cloud-a", replies: []string{reviewedTasks}}
	p := newTestPipeline(t, dir)
	p.Reviewer = rev

	out, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if out.Plan != planDoc || !out.Reviewed {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Tasks.Tasks) != 1 || out.Tasks.Tasks[0].Title != "t (reviewed)" {
		t.Fatalf("tasks = %+v (review fixes must be applied)", out.Tasks)
	}
	if out.Tasks.Epic != "i1" {
		t.Fatalf("epic = %q, want the job's item id", out.Tasks.Epic)
	}
	if plan.calls != 2 || rev.calls != 1 {
		t.Fatalf("calls = plan %d, review %d", plan.calls, rev.calls)
	}
	planPrompt, decomposePrompt := plan.prompts[0], plan.prompts[1]
	for _, want := range []string{"Add a /health endpoint", "HELLO README", "internal/", "(no description)", "high"} {
		if !strings.Contains(planPrompt, want) {
			t.Errorf("plan prompt missing %q:\n%s", want, planPrompt)
		}
	}
	for _, want := range []string{planDoc, "i1", "worker-x"} {
		if !strings.Contains(decomposePrompt, want) {
			t.Errorf("decompose prompt missing %q", want)
		}
	}
	if !strings.Contains(rev.prompts[0], oneTask) {
		t.Errorf("review prompt does not carry the task list:\n%s", rev.prompts[0])
	}
}

// TestPipelineDecomposeRetry: an invalid first reply is retried once with
// the validation error included, and the valid second reply wins.
func TestPipelineDecomposeRetry(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, badSizeTasks, validTasks}}
	p := newTestPipeline(t, "")

	out, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if out.Reviewed || len(out.Tasks.Tasks) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if plan.calls != 3 {
		t.Fatalf("planner calls = %d, want 3 (plan, bad decompose, retry)", plan.calls)
	}
	retry := plan.prompts[2]
	for _, want := range []string{"Your previous reply was invalid", "/tasks/0/size"} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry prompt missing %q:\n%s", want, retry)
		}
	}
}

// TestPipelineDecomposeRetriesExhausted: still invalid after the two
// retries fails the job (the worker's OnFail path then pings Discord).
func TestPipelineDecomposeRetriesExhausted(t *testing.T) {
	plan := &scriptModel{id: "planner-a",
		replies: []string{planDoc, badSizeTasks, badSizeTasks, badSizeTasks}}
	p := newTestPipeline(t, "")

	_, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err == nil || !strings.Contains(err.Error(), "still invalid after 2 retries") {
		t.Fatalf("err = %v, want retry exhaustion", err)
	}
	if plan.calls != 4 {
		t.Fatalf("planner calls = %d, want 4 (plan, decompose + 2 retries)", plan.calls)
	}
}

// TestPipelineDecomposeTransportError: a model (transport) error fails
// immediately instead of burning the retries.
func TestPipelineDecomposeTransportError(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc},
		err: errors.New("engine gone"), failAt: 1} // the decompose call, not the plan call
	p := newTestPipeline(t, "")

	_, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err == nil || !strings.Contains(err.Error(), "engine gone") {
		t.Fatalf("err = %v, want the transport error", err)
	}
	if plan.calls != 2 {
		t.Fatalf("planner calls = %d, want 2 (plan, failed decompose, no retries)", plan.calls)
	}
}

func TestPipelineReviewSkippedSameModel(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks, validTasks}}
	p := newTestPipeline(t, "")
	p.Reviewer = plan // the same model cannot review its own list

	out, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if out.Reviewed {
		t.Fatal("review ran on the planner's own model")
	}
	if plan.calls != 2 {
		t.Fatalf("planner calls = %d, want 2 (no review)", plan.calls)
	}
}

func TestPipelineReviewInvalidKeepsDecomposed(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	rev := &scriptModel{id: "cloud-a", replies: []string{badSizeTasks}}
	p := newTestPipeline(t, "")
	p.Reviewer = rev

	out, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err != nil {
		t.Fatalf("Plan: %v (a bad review must not fail the job)", err)
	}
	if out.Reviewed {
		t.Fatal("Reviewed must stay false when the review reply is invalid")
	}
	if len(out.Tasks.Tasks) != 1 || out.Tasks.Tasks[0].Title != "t" {
		t.Fatalf("tasks = %+v, want the decomposed list kept", out.Tasks)
	}
}

func TestPipelineReviewErrorKeepsDecomposed(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	rev := &scriptModel{id: "cloud-a", err: errors.New("cloud 503")}
	p := newTestPipeline(t, "")
	p.Reviewer = rev

	out, err := p.Plan(context.Background(), plan, testJob("planner-a"))
	if err != nil {
		t.Fatalf("Plan: %v (a review error must not fail the job)", err)
	}
	if out.Reviewed {
		t.Fatal("Reviewed must stay false when the review call fails")
	}
	if len(out.Tasks.Tasks) != 1 || out.Tasks.Tasks[0].Title != "t" {
		t.Fatalf("tasks = %+v, want the decomposed list kept", out.Tasks)
	}
}

// TestRepoSummaryPlaceholder: no repo root gives the plan step a
// placeholder instead of an error.
func TestRepoSummaryPlaceholder(t *testing.T) {
	plan := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	p := newTestPipeline(t, "")

	if _, err := p.Plan(context.Background(), plan, testJob("planner-a")); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.prompts[0], "(repo summary unavailable)") {
		t.Errorf("plan prompt missing repo placeholder:\n%s", plan.prompts[0])
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"Here you go:\n```json\n{\"a\":1}\n```\n", `{"a":1}`},
		{"no braces at all", "no braces at all"},
	}
	for _, tc := range cases {
		if got := extractJSONObject(tc.in); got != tc.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRender(t *testing.T) {
	got := render("a {{X}} and {{X}} and {{Y}}", map[string]string{"X": "1", "Y": "2"})
	if got != "a 1 and 1 and 2" {
		t.Fatalf("render = %q", got)
	}
}

// TestPipelineReplanComments: a re-plan's prompt carries the epic's
// comments as feedback for the planning model; a first plan gets the
// "(no comments)" placeholder.
func TestPipelineReplanComments(t *testing.T) {
	p := newTestPipeline(t, "")

	re := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	job := testJob("planner-a")
	job.Trigger = TriggerReplan
	job.Comments = []string{"Split task one in two.", "Drop the cache task."}
	if _, err := p.Plan(context.Background(), re, job); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, want := range []string{"Split task one in two.", "Drop the cache task."} {
		if !strings.Contains(re.prompts[0], want) {
			t.Errorf("re-plan prompt missing comment %q:\n%s", want, re.prompts[0])
		}
	}

	first := &scriptModel{id: "planner-a", replies: []string{planDoc, validTasks}}
	if _, err := p.Plan(context.Background(), first, testJob("planner-a")); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(first.prompts[0], "(no comments)") {
		t.Errorf("first plan prompt missing (no comments):\n%s", first.prompts[0])
	}
}
