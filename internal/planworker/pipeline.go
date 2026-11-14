package planworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// PlanOutput is what a planning pipeline produces for one job.
type PlanOutput struct {
	// Plan is the PLAN.md document (goal, architecture, files touched,
	// risks, open questions).
	Plan string
	// Tasks is the validated task list: the decompose step's output, with
	// the review step's fixes applied once when a reviewer was available.
	Tasks TaskList
	// Reviewed is true when a different reviewer model checked the list.
	Reviewed bool
}

// maxDecomposeRetries is how many times the decompose step retries a
// reply that fails schema validation, each retry carrying the parse error
// (PLAN.md, "Planning pipeline": retry up to 2 times, then fail the job).
const maxDecomposeRetries = 2

// Pipeline is the B3 planner. It runs the Plan, Decompose and Review
// prompts (prompts/*.md) as three model calls, each with a fresh context
// (docs/work-board/PLAN.md, "Planning pipeline"):
//
//  1. Plan (the job's planner model): epic + repo summary -> PLAN.md.
//  2. Decompose (same model): plan -> a JSON task list that must validate
//     against the task schema; an invalid reply is retried up to
//     maxDecomposeRetries times with the error included.
//  3. Review (p.Reviewer, a different model, when set): the task list ->
//     fixes, applied once.
type Pipeline struct {
	prompts  map[string]string // step filename -> template
	schema   *TaskSchema
	registry *registry.File
	catalog  *catalog.File
	repoRoot string

	// Reviewer is the "different model" for the review step, if one is
	// available (PLAN.md: "a different model if available, e.g. cloud
	// reviewing local or vice versa"). When nil, or when it is the same
	// model as the planner, the review step is skipped.
	Reviewer Model
}

// NewPipeline loads the three prompts and the task schema from dir and
// returns a Pipeline that runs the planning flow. repoRoot is the repo the
// plan step summarises for the model ("" gives a placeholder instead).
// cat is the capability catalog (config/capabilities.yaml), or nil when
// there is none — the decompose prompt then lists no capabilities and the
// planner cannot emit capability cards.
func NewPipeline(dir, repoRoot string, reg *registry.File, cat *catalog.File) (*Pipeline, error) {
	p := &Pipeline{
		prompts:  map[string]string{},
		registry: reg,
		catalog:  cat,
		repoRoot: repoRoot,
	}
	for _, name := range []string{"plan.md", "decompose.md", "review.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("load prompt %s: %w", name, err)
		}
		p.prompts[name] = string(data)
	}
	schData, err := os.ReadFile(filepath.Join(dir, SchemaFile))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", SchemaFile, err)
	}
	sch, err := NewTaskSchema(schData)
	if err != nil {
		return nil, err
	}
	p.schema = sch
	return p, nil
}

// Plan runs the three-step pipeline for job. model is the planner named in
// the job; the review step uses p.Reviewer when it is a different model.
func (p *Pipeline) Plan(ctx context.Context, model Model, job *Job) (*PlanOutput, error) {
	// Step 1: Plan (the planner model, the job's effort).
	repoSummary, err := p.repoSummary()
	if err != nil {
		return nil, fmt.Errorf("build repo summary: %w", err)
	}
	planPrompt := render(p.prompts["plan.md"], map[string]string{
		"EPIC_TITLE":       job.Item.Name,
		"EPIC_DESCRIPTION": epicDescription(job),
		"EPIC_COMMENTS":    epicComments(job),
		"REPO_SUMMARY":     repoSummary,
		"EFFORT":           jobEffort(job),
	})
	planDoc, err := p.chat(ctx, model, planPrompt)
	if err != nil {
		return nil, fmt.Errorf("plan step: %w", err)
	}

	// Step 2: Decompose (same model) -> a schema-validated task list.
	tasks, err := p.decompose(ctx, model, job, planDoc)
	if err != nil {
		return nil, err
	}

	// Step 3: Review (a different model, when available) -> fixes applied
	// once. A failed or invalid review keeps the decomposed list.
	out := &PlanOutput{Plan: planDoc, Tasks: tasks}
	if rev := p.reviewer(model); rev != nil {
		if reviewed, ok := p.review(ctx, rev, job, planDoc, tasks); ok {
			out.Tasks = reviewed
			out.Reviewed = true
		}
	}
	return out, nil
}

// decompose runs the decompose step and validates the reply against the
// task schema, retrying up to maxDecomposeRetries times with the
// validation error included. It fails the job after the last retry; a
// model (transport) error fails immediately rather than retrying.
func (p *Pipeline) decompose(ctx context.Context, model Model, job *Job, planDoc string) (TaskList, error) {
	base := render(p.prompts["decompose.md"], map[string]string{
		"EPIC_TITLE":      job.Item.Name,
		"EPIC_ID":         job.Item.ID,
		"PLAN":            planDoc,
		"EXECUTOR_MODELS": p.executorModels(),
		"CAPABILITIES":    p.catalogCapabilities(),
	})

	var lastErr error
	prompt := base
	for attempt := 0; attempt <= maxDecomposeRetries; attempt++ {
		out, err := p.chat(ctx, model, prompt)
		if err != nil {
			return TaskList{}, fmt.Errorf("decompose step: %w", err)
		}
		tl, verr := p.schema.Validate([]byte(extractJSONObject(out)))
		if verr == nil {
			tl.Epic = job.Item.ID
			return tl, nil
		}
		lastErr = verr
		if attempt == maxDecomposeRetries {
			break
		}
		prompt = base +
			"\n\nYour previous reply was invalid:\n" + ValidationErrorDetail(verr) +
			"\n\nReply again with ONLY a valid JSON object that matches the schema."
	}
	return TaskList{}, fmt.Errorf(
		"decompose step: task list still invalid after %d retries: %w",
		maxDecomposeRetries, lastErr)
}

// review runs the review step with rev and returns the reviewed list and
// whether the review produced a usable (schema-valid) result. A failed
// call or an invalid reply yields ok=false, so the caller keeps the
// pre-review list: the fixes are applied once, never retried.
func (p *Pipeline) review(ctx context.Context, rev Model, job *Job, planDoc string, tasks TaskList) (TaskList, bool) {
	prompt := render(p.prompts["review.md"], map[string]string{
		"EPIC_TITLE": job.Item.Name,
		"PLAN":       planDoc,
		"TASKS":      string(mustMarshal(tasks)),
	})
	out, err := p.chat(ctx, rev, prompt)
	if err != nil {
		return TaskList{}, false
	}
	reviewed, err := p.schema.Validate([]byte(extractJSONObject(out)))
	if err != nil {
		return TaskList{}, false
	}
	reviewed.Epic = job.Item.ID
	return reviewed, true
}

// reviewer returns the model the review step runs on: p.Reviewer when it is
// set and is a different model than the planner (PLAN.md: "a different
// model if available"), nil otherwise (which skips the review step).
func (p *Pipeline) reviewer(planner Model) Model {
	if p.Reviewer == nil || p.Reviewer.ID() == planner.ID() {
		return nil
	}
	return p.Reviewer
}

// chat runs one completion with a single user message; the prompts are
// self-contained, so no system message is needed.
func (p *Pipeline) chat(ctx context.Context, m Model, prompt string) (string, error) {
	out, err := m.Chat(ctx, []models.ChatMessage{{Role: "user", Content: prompt}})
	if err != nil {
		return "", err
	}
	return out, nil
}

// executorModels lists the enabled executor model ids for the decompose
// prompt, so the suggested executor_model is a real registry id.
func (p *Pipeline) executorModels() string {
	if p.registry == nil {
		return "(none configured)"
	}
	exec, err := p.registry.ByRole("executor")
	if err != nil || len(exec) == 0 {
		return "(none configured)"
	}
	ids := make([]string, 0, len(exec))
	for _, m := range exec {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

// catalogCapabilities renders the capability catalog for the decompose
// prompt, one line per capability: its name, where it can run, the cost
// (does it need the 4090 out of the brain), and its typical runtime — the
// same facts jarvis-mq caps shows the model. A nil or empty catalog gives
// a placeholder, in which case the prompt says no capability is available
// and the planner emits ordinary model-executed tasks.
func (p *Pipeline) catalogCapabilities() string {
	if p.catalog == nil || len(p.catalog.Capabilities) == 0 {
		return "(none configured)"
	}
	var b strings.Builder
	for _, name := range p.catalog.SortedNames() {
		c := p.catalog.Capabilities[name]
		rt, err := c.TypicalRuntime()
		typical := "?"
		if err == nil {
			typical = rt.Round(time.Second).String()
		}
		fmt.Fprintf(&b, "- %s: %s — where: %s, %s, typical ~%s\n",
			name, c.Description, strings.Join(c.Locations(), ","), c.Cost(), typical)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// repoSummary builds the plan step's repo input: a bounded file tree plus
// the README (PLAN.md, "Planning pipeline", step 1).
func (p *Pipeline) repoSummary() (string, error) {
	if p.repoRoot == "" {
		return "(repo summary unavailable)", nil
	}
	tree, err := fileTree(p.repoRoot)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("File tree:\n")
	b.WriteString(tree)
	if readme := readREADME(p.repoRoot); readme != "" {
		b.WriteString("\nREADME.md:\n")
		b.WriteString(readme)
	}
	return b.String(), nil
}

// fileTree returns an indented tree of root, capped at maxTreeLines and
// skipping junk directories, so a large repo cannot blow up the prompt.
func fileTree(root string) (string, error) {
	const (
		maxDepth = 4
		maxLines = 400
	)
	var b strings.Builder
	lines := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxDepth || lines >= maxLines {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if lines >= maxLines {
				return
			}
			if skipDir(e.Name()) {
				continue
			}
			indent := strings.Repeat("  ", depth)
			if e.IsDir() {
				b.WriteString(indent + e.Name() + "/\n")
				lines++
				walk(filepath.Join(dir, e.Name()), depth+1)
			} else {
				b.WriteString(indent + e.Name() + "\n")
				lines++
			}
		}
	}
	walk(root, 0)
	if lines >= maxLines {
		b.WriteString("  ...(truncated)\n")
	}
	return b.String(), nil
}

// skipDir reports whether a top-level or nested directory is junk for a
// planning summary: version control, build output, vendored code, model
// weights and the agent's own notes.
func skipDir(name string) bool {
	switch name {
	case ".git", ".claude", "node_modules", "vendor", "tmp", "build",
		"dist", "models", "notes", "__pycache__", ".cache", ".venv":
		return true
	}
	return false
}

// readREADME returns the repo's README (capped), or "" when there is none.
func readREADME(root string) string {
	const maxBytes = 4000
	for _, name := range []string{"README.md", "README"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		s := string(data)
		if len(s) > maxBytes {
			s = s[:maxBytes] + "\n...(truncated)"
		}
		return s
	}
	return ""
}

// render fills each {{KEY}} placeholder in template with vals[KEY].
func render(template string, vals map[string]string) string {
	for k, v := range vals {
		template = strings.ReplaceAll(template, "{{"+k+"}}", v)
	}
	return template
}

// extractJSONObject returns the substring of s from its first '{' to its
// last '}', tolerating code fences or stray text a model may add around the
// JSON. It returns s unchanged when it has no object to extract.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return s
	}
	j := strings.LastIndexByte(s, '}')
	if j < i {
		return s
	}
	return s[i : j+1]
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// A TaskList that validated cannot fail to marshal; this only
		// triggers on a caller bug, and an empty list is a safe fallback.
		return []byte(`{"epic":"","tasks":[]}`)
	}
	return b
}

// jobEffort returns the job's planning effort, defaulting to high (the
// bridge always sets it; this is defensive).
func jobEffort(job *Job) string {
	if job.Effort == "" {
		return "high"
	}
	return job.Effort
}

// epicDescription returns the epic's description, or a placeholder when the
// job did not carry one.
func epicDescription(job *Job) string {
	if job.Item.Description == "" {
		return "(no description)"
	}
	return job.Item.Description
}

// epicComments returns the re-plan feedback (B5) as one block, oldest
// first, or a placeholder for a first plan (the bridge only carries
// comments on replan_epic jobs).
func epicComments(job *Job) string {
	if len(job.Comments) == 0 {
		return "(no comments)"
	}
	return strings.Join(job.Comments, "\n")
}
