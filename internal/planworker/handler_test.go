package planworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// testRegistry is the fixture registry: two local planners, one cloud
// planner, and an executor that must not be offered as a planner.
const testRegistry = `
models:
  - id: planner-a
    display_name: Planner A
    location: local
    engine: ollama
    endpoint: http://local-a
    roles: [planner]
    context_tokens: 32768
    vram_gb: 10
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: true
    home_location: gpu
    fallback_location: cpu
  - id: planner-off
    display_name: Planner Off
    location: local
    engine: ollama
    endpoint: http://local-b
    roles: [planner]
    context_tokens: 32768
    vram_gb: 10
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: false
    home_location: gpu
    fallback_location: none
  - id: cloud-a
    display_name: Cloud A
    location: cloud
    engine: anthropic
    endpoint: https://api.anthropic.com/v1
    roles: [planner]
    context_tokens: 200000
    vram_gb: 0
    ram_gb: 0
    thinking: "thinking.budget_tokens:16384"
    cost_per_mtok: 3
    enabled: true
    home_location: cloud
    fallback_location: none
  - id: worker-x
    display_name: Worker X
    location: local
    engine: ollama
    endpoint: http://local-c
    roles: [executor]
    context_tokens: 32768
    vram_gb: 10
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: true
    home_location: gpu
    fallback_location: none
`

func testFile(t *testing.T) *registry.File {
	t.Helper()
	f, err := registry.Parse([]byte(testRegistry))
	if err != nil {
		t.Fatalf("parse fixture registry: %v", err)
	}
	return f
}

func testJob(modelID string) *Job {
	return &Job{
		Trigger: TriggerPlan,
		Board:   Board{URL: "http://plane", ProjectID: "p1"},
		Item:    Item{ID: "i1", Name: "Add a /health endpoint"},
		ModelID: modelID,
		Effort:  "high",
	}
}

// stubEngine is a stub model engine: it records every call so tests can
// assert the exact swap sequence, and it tracks its loaded models.
type stubEngine struct {
	loaded    []string
	calls     []string
	chatOut   string
	chatErr   error
	failLoads map[string]error // per-model Load failures
}

func (s *stubEngine) Loaded(ctx context.Context) ([]string, error) {
	out := make([]string, len(s.loaded))
	copy(out, s.loaded)
	return out, nil
}

func (s *stubEngine) Load(ctx context.Context, model string) error {
	s.calls = append(s.calls, "load:"+model)
	if err := s.failLoads[model]; err != nil {
		return err
	}
	if !contains(s.loaded, model) {
		s.loaded = append(s.loaded, model)
	}
	return nil
}

func (s *stubEngine) Unload(ctx context.Context, model string) error {
	s.calls = append(s.calls, "unload:"+model)
	for i, n := range s.loaded {
		if n == model {
			s.loaded = append(s.loaded[:i], s.loaded[i+1:]...)
			break
		}
	}
	return nil
}

func (s *stubEngine) Chat(ctx context.Context, model string, messages []models.ChatMessage) (string, error) {
	s.calls = append(s.calls, "chat:"+model)
	if s.chatErr != nil {
		return "", s.chatErr
	}
	return s.chatOut, nil
}

// stubPlanner is a stub planning pipeline: one chat call, the reply is
// the plan (no tasks).
type stubPlanner struct{ gotPrompt string }

func (p *stubPlanner) Plan(ctx context.Context, model Model, job *Job) (*PlanOutput, error) {
	out, err := model.Chat(ctx, []models.ChatMessage{{Role: "user", Content: "plan " + job.Item.Name}})
	p.gotPrompt = job.Item.Name
	return &PlanOutput{Plan: out}, err
}

// stubCloud is a stub cloud API.
type stubCloud struct {
	calls []string
	out   string
	err   error
}

func (c *stubCloud) Chat(ctx context.Context, m *registry.Model, messages []models.ChatMessage) (string, error) {
	c.calls = append(c.calls, m.ID)
	return c.out, c.err
}

func newTestHandler(t *testing.T, eng *stubEngine, cloud *stubCloud) *Handler {
	t.Helper()
	return &Handler{
		Models:   testFile(t),
		Engines:  func(string) Engine { return eng },
		Cloud:    cloud,
		Planner:  &stubPlanner{},
		FrontEnd: "frontend-3b",
	}
}

func assertCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

func neverUnloaded(t *testing.T, calls []string, names ...string) {
	t.Helper()
	for _, c := range calls {
		for _, n := range names {
			if c == "unload:"+n {
				t.Fatalf("front-end model %s was unloaded (calls %v)", n, calls)
			}
		}
	}
}

// TestRunLocalSwap is the B2 acceptance path: the planner is not loaded,
// so the handler unloads the current model, loads the planner, plans,
// then unloads the planner and restores the previous model. The
// front-end model is never touched.
func TestRunLocalSwap(t *testing.T) {
	eng := &stubEngine{loaded: []string{"brain-27b", "frontend-3b"}, chatOut: "# Plan\n..."}
	cloud := &stubCloud{}
	h := newTestHandler(t, eng, cloud)

	res, err := h.Run(context.Background(), testJob("planner-a"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Plan != "# Plan\n..." || !res.Local || res.Model.ID != "planner-a" {
		t.Fatalf("result = %+v", res)
	}
	assertCalls(t, eng.calls,
		"unload:brain-27b", "load:planner-a", "chat:planner-a",
		"unload:planner-a", "load:brain-27b")
	neverUnloaded(t, eng.calls, "frontend-3b")
	assertCalls(t, eng.loaded, "frontend-3b", "brain-27b")
	if len(cloud.calls) != 0 {
		t.Fatalf("cloud was called for a local job: %v", cloud.calls)
	}
}

// TestRunLocalAlreadyLoaded: no swap when the planner is already loaded —
// no unload, no load, no restore.
func TestRunLocalAlreadyLoaded(t *testing.T) {
	eng := &stubEngine{loaded: []string{"planner-a", "frontend-3b"}, chatOut: "plan"}
	h := newTestHandler(t, eng, &stubCloud{})

	if _, err := h.Run(context.Background(), testJob("planner-a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCalls(t, eng.calls, "chat:planner-a")
	assertCalls(t, eng.loaded, "planner-a", "frontend-3b")
}

// TestRunLocalOnlyFrontEndLoaded: the engine holds only the front-end
// model; the swap loads the planner without unloading anything and
// leaves no restore to do.
func TestRunLocalOnlyFrontEndLoaded(t *testing.T) {
	eng := &stubEngine{loaded: []string{"frontend-3b"}, chatOut: "plan"}
	h := newTestHandler(t, eng, &stubCloud{})

	if _, err := h.Run(context.Background(), testJob("planner-a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCalls(t, eng.calls, "load:planner-a", "chat:planner-a", "unload:planner-a")
	neverUnloaded(t, eng.calls, "frontend-3b")
	assertCalls(t, eng.loaded, "frontend-3b")
}

// TestRunLocalLoadFailureRestores: when loading the planner fails, the
// previous model is restored and the job fails.
func TestRunLocalLoadFailureRestores(t *testing.T) {
	eng := &stubEngine{
		loaded:    []string{"brain-27b", "frontend-3b"},
		failLoads: map[string]error{"planner-a": errors.New("no space left in vram")},
	}
	h := newTestHandler(t, eng, &stubCloud{})

	_, err := h.Run(context.Background(), testJob("planner-a"))
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("Run err = %v, want load failure", err)
	}
	assertCalls(t, eng.calls, "unload:brain-27b", "load:planner-a", "load:brain-27b")
	assertCalls(t, eng.loaded, "frontend-3b", "brain-27b")
}

// TestRunLocalRestoreFailure: when the restore after a failed load also
// fails, both errors are reported.
func TestRunLocalRestoreFailure(t *testing.T) {
	eng := &stubEngine{
		loaded: []string{"brain-27b", "frontend-3b"},
		failLoads: map[string]error{
			"planner-a": errors.New("no space left in vram"),
			"brain-27b": errors.New("restore timed out"),
		},
	}
	h := newTestHandler(t, eng, &stubCloud{})

	_, err := h.Run(context.Background(), testJob("planner-a"))
	if err == nil || !strings.Contains(err.Error(), "no space left") ||
		!strings.Contains(err.Error(), "restore brain-27b") {
		t.Fatalf("Run err = %v, want load and restore failures", err)
	}
	assertCalls(t, eng.calls, "unload:brain-27b", "load:planner-a", "load:brain-27b")
}

// TestRunPlanFailureStillRestores: a failed planning call still unloads
// the swapped-in planner and restores the previous model.
func TestRunPlanFailureStillRestores(t *testing.T) {
	eng := &stubEngine{
		loaded:  []string{"brain-27b", "frontend-3b"},
		chatErr: errors.New("engine died mid-plan"),
	}
	h := newTestHandler(t, eng, &stubCloud{})

	_, err := h.Run(context.Background(), testJob("planner-a"))
	if err == nil || !strings.Contains(err.Error(), "engine died mid-plan") {
		t.Fatalf("Run err = %v, want plan failure", err)
	}
	assertCalls(t, eng.calls,
		"unload:brain-27b", "load:planner-a", "chat:planner-a",
		"unload:planner-a", "load:brain-27b")
	assertCalls(t, eng.loaded, "frontend-3b", "brain-27b")
}

// TestRunCloud is the B2 cloud path: the job runs on the cloud API and
// the local engine is not touched at all.
func TestRunCloud(t *testing.T) {
	eng := &stubEngine{loaded: []string{"brain-27b", "frontend-3b"}}
	cloud := &stubCloud{out: "cloud plan"}
	h := newTestHandler(t, eng, cloud)

	res, err := h.Run(context.Background(), testJob("cloud-a"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Plan != "cloud plan" || res.Local || res.Model.ID != "cloud-a" {
		t.Fatalf("result = %+v", res)
	}
	assertCalls(t, cloud.calls, "cloud-a")
	assertCalls(t, eng.calls) // the engine must never be touched
	assertCalls(t, eng.loaded, "brain-27b", "frontend-3b")
}

// TestRunCloudFailure: a cloud failure surfaces and still touches no
// engine.
func TestRunCloudFailure(t *testing.T) {
	eng := &stubEngine{loaded: []string{"brain-27b"}}
	cloud := &stubCloud{err: errors.New("401 invalid x-api-key")}
	h := newTestHandler(t, eng, cloud)

	_, err := h.Run(context.Background(), testJob("cloud-a"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Run err = %v, want cloud failure", err)
	}
	assertCalls(t, eng.calls)
}

// TestRunModelGate: unknown, disabled and non-planner models are
// rejected before any engine or cloud call.
func TestRunModelGate(t *testing.T) {
	for _, tc := range []struct {
		id, want string
	}{
		{"no-such", "not in registry"},
		{"planner-off", "disabled"},
		{"worker-x", "not tagged planner"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			eng := &stubEngine{loaded: []string{"brain-27b"}}
			cloud := &stubCloud{}
			h := newTestHandler(t, eng, cloud)
			_, err := h.Run(context.Background(), testJob(tc.id))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run err = %v, want %q", err, tc.want)
			}
			assertCalls(t, eng.calls)
			assertCalls(t, cloud.calls)
		})
	}
}

// TestParse checks the B1 contract decoding.
func TestParse(t *testing.T) {
	const good = `{
		"trigger": "plan_epic",
		"board": {"url": "http://plane", "workspace": "jarvis",
			"project": "T2I", "project_id": "p1"},
		"item": {"id": "i1", "name": "Epic", "sequence_id": 3,
			"parent": "", "state": "Planning", "labels": ["epic", "plan:planner-a"]},
		"model_id": "planner-a",
		"effort": "medium"
	}`
	j, err := Parse(good)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if j.ModelID != "planner-a" || j.Effort != "medium" ||
		j.Board.Project != "T2I" || j.Item.Name != "Epic" || j.Item.SequenceID != 3 {
		t.Fatalf("parsed = %+v", j)
	}

	// B5: a re-plan job parses like a plan job and carries the epic's
	// comments as feedback.
	const replan = `{
		"trigger": "replan_epic",
		"item": {"id": "i1", "name": "Epic"},
		"model_id": "planner-a",
		"effort": "medium",
		"comments": ["Split task one in two.", "Drop the cache task."]
	}`
	rp, err := Parse(replan)
	if err != nil {
		t.Fatalf("Parse re-plan: %v", err)
	}
	if rp.Trigger != TriggerReplan || len(rp.Comments) != 2 ||
		rp.Comments[0] != "Split task one in two." {
		t.Fatalf("re-plan parsed = %+v", rp)
	}

	for _, tc := range []struct {
		name, in, want string
	}{
		{"other trigger", `{"trigger":"run_task","item":{"id":"i"},"model_id":"m"}`, "not a planning job"},
		{"no model", `{"trigger":"plan_epic","item":{"id":"i"}}`, "no model_id"},
		{"garbage", `not json`, "parse plan job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse err = %v, want %q", err, tc.want)
			}
		})
	}
}
