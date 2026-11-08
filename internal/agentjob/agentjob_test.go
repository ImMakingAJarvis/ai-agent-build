package agentjob

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/power"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ingest(t *testing.T, s *Store, sp Spec) {
	t.Helper()
	if _, err := s.Ingest(sp); err != nil {
		t.Fatal(err)
	}
}

func runner(t *testing.T, s *Store, script string) *Runner {
	h, err := queue.NewHistory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return &Runner{
		Store: s, History: h,
		Command:  func(driver, prompt, effort string) *exec.Cmd { return exec.Command("sh", "-c", script) },
		PRLookup:   func(string) string { return "" },
		Brief:      func(string) string { return "RULES" },
		StackStatus: func() string { return "" },
	}
}

func TestRunRecordsOutputAndPR(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a", Branch: "b"})
	r := runner(t, s, `echo working; echo https://github.com/o/r/pull/7`)
	j, err := r.Run(context.Background(), "a", "r1", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Done || j.PRURL != "https://github.com/o/r/pull/7" || j.Duration == "" {
		t.Fatalf("bad job: %+v", j.State)
	}
	got, _ := s.Get("a")
	if got.Status != Done || got.PRURL == "" {
		t.Fatalf("state not stored: %+v", got.State)
	}
	if b, _ := os.ReadFile(filepath.Join(s.JobDir("a"), "transcript.txt")); !strings.Contains(string(b), "working") {
		t.Fatalf("transcript: %q", b)
	}
	hist, _ := os.ReadFile(filepath.Join(os.Getenv("JARVIS_DATA_DIR"), "jobs.jsonl"))
	if !strings.Contains(string(hist), `"kind":"agent"`) || !strings.Contains(string(hist), `"status":"completed"`) {
		t.Fatalf("history: %s", hist)
	}
}

func TestFailingAgentRecordedFailed(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A"})
	r := runner(t, s, `echo boom; exit 3`)
	if _, err := r.Run(context.Background(), "a", "r1", "local", ""); err == nil {
		t.Fatal("want error")
	}
	got, _ := s.Get("a")
	if got.Status != Failed || !strings.Contains(got.Error, "status 3") || !strings.Contains(got.Summary, "boom") {
		t.Fatalf("got %+v", got.State)
	}
}

func TestRunRecordsPowerCost(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	t.Setenv("JARVIS_POWER_RATE", "0.25")
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A"})
	r := runner(t, s, `true`)
	// Fake sampler: 1000 Wh (1 kWh) for the whole round.
	r.Power = func(jobDir string) func() float64 {
		return func() float64 { return 1000 }
	}
	j, err := r.Run(context.Background(), "a", "r1", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Kwh != 1 || j.PowerCost != 0.25 {
		t.Fatalf("power: kwh=%v cost=%v, want 1 / 0.25", j.Kwh, j.PowerCost)
	}
	got, _ := s.Get("a")
	if got.Kwh != 1 || got.PowerCost != 0.25 {
		t.Fatalf("state not stored: %+v", got.State)
	}
	hist, _ := os.ReadFile(filepath.Join(os.Getenv("JARVIS_DATA_DIR"), "jobs.jsonl"))
	if !strings.Contains(string(hist), `"kwh":1`) || !strings.Contains(string(hist), `"power_cost":0.25`) {
		t.Fatalf("history: %s", hist)
	}
}

func TestFinishUsesPersistedPower(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	t.Setenv("JARVIS_POWER_RATE", "0.10")
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A"})
	if _, err := s.Claim("a", "r1", "local"); err != nil {
		t.Fatal(err)
	}
	// Simulate the round having run (and sampled) until a reboot.
	j, _ := s.Get("a")
	j.Status = Running
	j.StartedAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := s.SetStatus("a", j.State); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.JobDir("a"), power.PersistFile),
		[]byte(`{"wh":7200}`), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := runner(t, s, `true`).Finish("a", 0, "done")
	if err != nil {
		t.Fatal(err)
	}
	// 7200 Wh = 7.2 kWh, at $0.10/kWh = $0.72.
	if notApprox(j.Kwh, 7.2) || notApprox(j.PowerCost, 0.72) {
		t.Fatalf("power: kwh=%v cost=%v, want 7.2 / 0.72", j.Kwh, j.PowerCost)
	}
}

// notApprox reports whether a and b differ by more than 1e-9.
func notApprox(a, b float64) bool {
	d := a - b
	return d < -1e-9 || d > 1e-9
}

func TestDefaultCommandMapsEffort(t *testing.T) {
	cmd := defaultCommand("local", "do it", "unknown")
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "--reasoning-effort") {
		t.Fatalf("Claude Code 2.1 rejects --reasoning-effort: %s", joined)
	}
	if !strings.Contains(joined, "--effort") || !strings.Contains(joined, "medium") {
		t.Fatalf("unknown coding job should --effort medium: %s", joined)
	}
	off := defaultCommand("local", "do it", "off")
	if strings.Contains(strings.Join(off.Args, " "), "--effort") {
		t.Fatalf("off must omit --effort: %s", off.Args)
	}
	env := strings.Join(off.Env, "\n")
	if !strings.Contains(env, "CLAUDE_CODE_DISABLE_THINKING=1") {
		t.Fatal("off should disable thinking")
	}
}

// TestDefaultCommandSetsJarvisModel is the P6 hook: every round's env
// carries JARVIS_MODEL with the model driving it, so the agent can prefix
// its board comments with the model name. Stale inherited values are
// replaced, not duplicated.
func TestDefaultCommandSetsJarvisModel(t *testing.T) {
	t.Setenv("JARVIS_MODEL", "stale-model")
	t.Setenv("LOCAL_MODEL", "")

	cmd := defaultCommand("local", "do it", "off")
	n := 0
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "JARVIS_MODEL="); ok {
			n++
			if v != "jarvis-brain" {
				t.Errorf("JARVIS_MODEL = %q, want jarvis-brain", v)
			}
		}
	}
	if n != 1 {
		t.Errorf("JARVIS_MODEL appears %d times in env, want 1", n)
	}

	if got := ModelFor("sonnet"); got != "sonnet" {
		t.Errorf("ModelFor(sonnet) = %q, want sonnet", got)
	}
	cmd = defaultCommand("sonnet", "do it", "off")
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "JARVIS_MODEL=sonnet") {
		t.Error("sonnet driver env lacks JARVIS_MODEL=sonnet")
	}
}

func TestModelForIgpu(t *testing.T) {
	t.Setenv("LOCAL_MODEL", "jarvis-brain")
	if got := ModelFor("igpu"); got != "jarvis-igpu" {
		t.Errorf("ModelFor(igpu) = %q, want jarvis-igpu", got)
	}
	t.Setenv("IGPU_MODEL", "qwen3-30b-a3b")
	if got := ModelFor("igpu"); got != "qwen3-30b-a3b" {
		t.Errorf("ModelFor(igpu) with IGPU_MODEL = %q", got)
	}
	// local/sonnet are unaffected by the igpu env.
	t.Setenv("IGPU_URL", "http://elsewhere:1")
	t.Setenv("IGPU_CONTEXT_TOKENS", "1")
	if got := ModelFor("local"); got != "jarvis-brain" {
		t.Errorf("ModelFor(local) = %q, want jarvis-brain", got)
	}
}

// TestDefaultCommandIgpuDriver is the acceptance test for the iGPU
// driver: the round runs through agent-local (not the API claude binary)
// pointed at the iGPU engine, with a context cap that fits the engine's
// --ctx-size, and with stale inherited values for the overridden vars
// removed so the child sees each exactly once.
func TestDefaultCommandIgpuDriver(t *testing.T) {
	// A stale brain URL must not leak into the igpu command.
	t.Setenv("ENGINE_URL", "http://stale:11434")
	t.Setenv("LOCAL_MODEL", "jarvis-brain")
	t.Setenv("LOCAL_CONTEXT_TOKENS", "131072")
	cmd := defaultCommand("igpu", "do it", "off")
	if cmd.Args[0] != "agent-local" {
		t.Fatalf("igpu driver must run agent-local, got %q", cmd.Args[0])
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{
		"ENGINE_URL=http://localhost:11435",
		"LOCAL_MODEL=jarvis-igpu",
		"LOCAL_CONTEXT_TOKENS=32768",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=16384",
	} {
		if !strings.Contains(env, "\n"+want+"\n") && !strings.HasPrefix(env, want+"\n") && !strings.HasSuffix(env, "\n"+want) {
			t.Errorf("env lacks %q in %s", want, env)
		}
	}
	for _, stale := range []string{"http://stale:11434", "LOCAL_MODEL=jarvis-brain", "LOCAL_CONTEXT_TOKENS=131072"} {
		if strings.Contains(env, stale) {
			t.Errorf("stale value %q leaked into env", stale)
		}
	}
	// Each overridden key appears exactly once.
	for _, key := range []string{"ENGINE_URL", "LOCAL_MODEL", "LOCAL_CONTEXT_TOKENS"} {
		n := 0
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, key+"=") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s appears %d times in env, want 1", key, n)
		}
	}
	// Overrides are honoured.
	t.Setenv("IGPU_URL", "http://deep:11436")
	t.Setenv("IGPU_CONTEXT_TOKENS", "65536")
	cmd = defaultCommand("igpu", "do it", "off")
	env = strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "ENGINE_URL=http://deep:11436") ||
		!strings.Contains(env, "LOCAL_CONTEXT_TOKENS=65536") {
		t.Errorf("IGPU_URL/IGPU_CONTEXT_TOKENS not honoured:\n%s", env)
	}
}

// TestRunPreservesDriverEnv is the regression test for the igpu photo
// rounds dying with ECONNREFUSED: Run() used to replace the command's env
// with the process environment (to add JARVIS_JOB_ID), which dropped the
// igpu driver's ENGINE_URL/LOCAL_MODEL overrides — every "igpu" round
// silently ran the brain model instead. The driver's env must survive the
// tag, with the stale inherited values still stripped.
func TestRunPreservesDriverEnv(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	// Stale brain values in the process env, as the loop inherits them.
	t.Setenv("ENGINE_URL", "http://stale:11434")
	t.Setenv("LOCAL_MODEL", "jarvis-brain")
	s := newStore(t)
	ingest(t, s, Spec{ID: "igpu-job", Title: "T", Instructions: "do"})
	r := &Runner{
		Store: s,
		Command: func(driver, prompt, effort string) *exec.Cmd {
			// The real driver command, with only the binary swapped for
			// something that prints the env it was given.
			c := defaultCommand(driver, prompt, effort)
			c.Path, c.Args = "/bin/sh", []string{"/bin/sh", "-c", "printenv"}
			return c
		},
		PRLookup:    func(string) string { return "" },
		Brief:       func(string) string { return "RULES" },
		StackStatus: func() string { return "" },
	}
	j, err := r.Run(context.Background(), "igpu-job", "r1", "igpu", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Done {
		t.Fatalf("job status %q, want done", j.Status)
	}
	out, _ := os.ReadFile(filepath.Join(s.JobDir("igpu-job"), "transcript.txt"))
	env := string(out)
	for _, want := range []string{
		"ENGINE_URL=http://localhost:11435",
		"LOCAL_MODEL=jarvis-igpu",
		"JARVIS_JOB_ID=igpu-job",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("run env lacks %q", want)
		}
	}
	for _, stale := range []string{"http://stale:11434", "LOCAL_MODEL=jarvis-brain"} {
		if strings.Contains(env, stale) {
			t.Errorf("stale value %q leaked into run env", stale)
		}
	}
}

// TestDefaultCommandSuperagentDriver is the acceptance test for the
// superagent driver: the round runs the superagent CLI (its own
// plan→act→verify loop, not the Claude Code harness) with the round's
// prompt as its goal, and the env carries the model that drives it.
func TestDefaultCommandSuperagentDriver(t *testing.T) {
	t.Setenv("JARVIS_MODEL", "stale-model")
	t.Setenv("LOCAL_MODEL", "")
	cmd := defaultCommand("superagent", "do it", "off")
	if cmd.Args[0] != "superagent" || len(cmd.Args) != 2 || cmd.Args[1] != "do it" {
		t.Fatalf("superagent driver args = %v", cmd.Args)
	}
	if got := ModelFor("superagent"); got != "jarvis-brain" {
		t.Errorf("ModelFor(superagent) = %q, want jarvis-brain", got)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "JARVIS_MODEL=jarvis-brain") {
		t.Error("superagent driver env lacks JARVIS_MODEL=jarvis-brain")
	}
	// No Claude Code harness flags may leak into the superagent command.
	joined := strings.Join(cmd.Args, " ")
	for _, no := range []string{"--dangerously-skip-permissions", "--output-format", "-p"} {
		if strings.Contains(joined, no) {
			t.Errorf("superagent args must not carry %q: %s", no, joined)
		}
	}
	// SUPERAGENT_MODEL pins the engine model.
	t.Setenv("SUPERAGENT_MODEL", "qwen-coder")
	if got := ModelFor("superagent"); got != "qwen-coder" {
		t.Errorf("ModelFor(superagent) with SUPERAGENT_MODEL = %q", got)
	}
}

func TestPromptHasRulesJobAndNote(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir()) // don't drain the real inbox
	r := &Runner{Brief: func(string) string { return "RULES" }, StackStatus: func() string { return "" }}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT", Branch: "br"}}, "local", "NOTE")
	for _, w := range []string{"RULES", "d0", "TEXT", "br", "NOTE"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q", w)
		}
	}
}

func TestPromptIncludesMachineSnapshot(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	snap := "STACK 2026-11-05\nOK: 3 engine(s) serving"
	r := &Runner{
		Brief:       func(string) string { return "RULES" },
		StackStatus: func() string { return snap },
	}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT"}}, "local", "")
	for _, w := range []string{"MACHINE SNAPSHOT at round start", snap} {
		if !strings.Contains(p, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, p)
		}
	}
	// Order: brief, then snapshot, then the job line.
	bi, si, ji := strings.Index(p, "RULES"), strings.Index(p, "MACHINE SNAPSHOT"), strings.Index(p, "=== YOUR JOB THIS ROUND")
	if !(bi < si && si < ji) {
		t.Fatalf("bad ordering: brief=%d snapshot=%d job=%d", bi, si, ji)
	}
}

func TestPromptOmitsEmptySnapshot(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	r := &Runner{Brief: func(string) string { return "RULES" }, StackStatus: func() string { return "" }}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT"}}, "local", "")
	if strings.Contains(p, "MACHINE SNAPSHOT") {
		t.Fatal("empty snapshot must not add a section")
	}
}

func TestPromptIncludesIdleOwnerMessages(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	if _, err := inbox.Send("hello from the owner"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Brief: func(string) string { return "RULES" }, StackStatus: func() string { return "" }}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT"}}, "local", "")
	if !strings.Contains(p, "hello from the owner") ||
		!strings.Contains(p, "while the agent was idle") {
		t.Fatalf("prompt lacks idle message:\n%s", p)
	}
	// Drained: the next round does not repeat it.
	p2 := r.Prompt(Job{Spec: Spec{ID: "d1", Title: "T", Instructions: "TEXT"}}, "local", "")
	if strings.Contains(p2, "hello from the owner") {
		t.Fatal("idle message repeated in next prompt")
	}
}

func TestNextSkipsUnmetDependencies(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "first", Priority: 10})
	ingest(t, s, Spec{ID: "blocked", Priority: 9, DependsOn: []string{"first"}})
	ingest(t, s, Spec{ID: "free", Priority: 1})
	j, _ := s.Next()
	if j == nil || j.ID != "first" {
		t.Fatalf("want first, got %v", j)
	}
	// Claim "first" (owner alive): blocked still waits, free is next.
	if _, err := s.Claim("first", "r", "local"); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Next()
	if j == nil || j.ID != "free" {
		t.Fatalf("want free, got %v", j)
	}
	s.SetStatus("first", State{Status: Done})
	j, _ = s.Next()
	if j == nil || j.ID != "blocked" {
		t.Fatalf("want blocked after dep done, got %v", j)
	}
}

// TestClassOrdering checks the queue rule: fix jobs always run before build
// jobs, build before background, regardless of numeric priority; within a
// class, higher priority first, then oldest.
func TestClassOrdering(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "bg", Class: ClassBackground, Priority: 1000})
	ingest(t, s, Spec{ID: "build", Class: ClassBuild, Priority: 100})
	ingest(t, s, Spec{ID: "fix", Class: ClassFix, Priority: 1}) // lowest number, must still run first
	// Legacy jobs (no class): migrate-sourced is background, the rest build.
	ingest(t, s, Spec{ID: "legacy-mig", Source: "migrate", Priority: 999})
	ingest(t, s, Spec{ID: "legacy-chat", Source: "discord", Priority: 80})

	want := []string{"fix", "build", "legacy-chat", "bg", "legacy-mig"}
	for i, id := range want {
		j, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if j == nil || j.ID != id {
			t.Fatalf("step %d: want %s, got %+v", i, id, j)
		}
		if err := s.SetStatus(id, State{Status: Skipped}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestClassOrderingWithinClass checks tie-breaks inside one class:
// priority desc, then created_at asc, then ID.
func TestClassOrderingWithinClass(t *testing.T) {
	s := newStore(t)
	old := time.Now().Add(-time.Hour)
	ingest(t, s, Spec{ID: "a", Class: ClassBuild, Priority: 5, CreatedAt: old})
	ingest(t, s, Spec{ID: "b", Class: ClassBuild, Priority: 9, CreatedAt: old.Add(time.Minute)})
	ingest(t, s, Spec{ID: "c", Class: ClassBuild, Priority: 5, CreatedAt: old.Add(time.Minute)})
	jobs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, j.ID)
	}
	if strings.Join(got, ",") != "b,a,c" {
		t.Fatalf("order %v, want [b a c]", got)
	}
}

// TestApplyOrderReordersAndReclassifies checks the persistence behind the
// dashboard's drag queue: ApplyOrder rewrites a job's class and within-class
// priority so the queue runs in the new top-to-bottom order within each band,
// and dropping a job into another band reclassifies it (band priority stays
// locked: build still runs before background).
func TestApplyOrderReordersAndReclassifies(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	// Start in the "wrong" order so the reorder is observable.
	ingest(t, s, Spec{ID: "b2", Class: ClassBuild, Priority: 9, CreatedAt: now})
	ingest(t, s, Spec{ID: "b1", Class: ClassBuild, Priority: 1, CreatedAt: now})
	ingest(t, s, Spec{ID: "g2", Class: ClassBackground, Priority: 9, CreatedAt: now})
	ingest(t, s, Spec{ID: "g1", Class: ClassBackground, Priority: 1, CreatedAt: now})

	// Reorder the build band to b1,b2 and promote background job g1 into it.
	if err := s.ApplyOrder(map[string][]string{
		ClassBuild:      {"b1", "b2", "g1"},
		ClassBackground: {"g2"},
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	for {
		j, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		got = append(got, j.ID)
		if err := s.SetStatus(j.ID, State{Status: Skipped}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "b1,b2,g1,g2" {
		t.Fatalf("run order %v, want [b1 b2 g1 g2]", got)
	}
	if g1, _ := s.Get("g1"); g1.Class != ClassBuild {
		t.Fatalf("g1 class %q, want %q", g1.Class, ClassBuild)
	}
	if g2, _ := s.Get("g2"); g2.Class != ClassBackground {
		t.Fatalf("g2 class %q, want %q", g2.Class, ClassBackground)
	}

	if err := s.ApplyOrder(map[string][]string{"nope": {"b1"}}); err == nil {
		t.Fatal("ApplyOrder accepted an unknown class")
	}
}

func TestSetDriverRewritesSpec(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "p1", Class: ClassBuild})
	if err := s.SetDriver("p1", "igpu"); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Driver != "igpu" {
		t.Fatalf("driver %q, want igpu", j.Driver)
	}
	// The driver rewrite must not touch the state.
	if j.Status != Ready {
		t.Fatalf("status %q, want ready", j.Status)
	}
	if err := s.SetDriver("p1", "nope"); err == nil {
		t.Fatal("SetDriver accepted an unknown driver")
	}
}

func TestNextExcluding(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	ingest(t, s, Spec{ID: "b"})
	j, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "a" {
		t.Fatalf("Next got %v, want a", j)
	}
	j, err = s.NextExcluding(map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "b" {
		t.Fatalf("NextExcluding(a) got %v, want b", j)
	}
	j, err = s.NextExcluding(map[string]bool{"a": true, "b": true})
	if err != nil {
		t.Fatal(err)
	}
	if j != nil {
		t.Fatalf("NextExcluding(a,b) got %v, want nil", j)
	}
}

func TestClaimedJobReadyAfterRestartAndNotRunTwice(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	if _, err := s.Claim("a", "r", "local"); err != nil {
		t.Fatal(err)
	}
	// Owner alive (this process): second claim refused, not ready.
	if _, err := s.Claim("a", "r2", "local"); err == nil {
		t.Fatal("second claim must fail while the owner is alive")
	}
	if j, _ := s.Next(); j != nil {
		t.Fatalf("claimed job offered: %v", j.ID)
	}
	// Owner died (crash/reboot): the next start sees it ready again.
	st := State{Status: Running, PID: 2147483646}
	s.SetStatus("a", st)
	j, _ := s.Next()
	if j == nil || j.ID != "a" {
		t.Fatal("dead owner's job should be ready again")
	}
	if _, err := s.Claim("a", "r3", "local"); err != nil {
		t.Fatal(err)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	s := newStore(t)
	ingest(t, s, Spec{ID: "a"})
	s.SetStatus("a", State{Status: Done})
	added, err := s.Ingest(Spec{ID: "a"}) // redelivered message
	if err != nil || added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if j, _ := s.Get("a"); j.Status != Done {
		t.Fatal("redelivery reset state")
	}
}

const sample = `# T

## Phase 1 — Hardware
- [ ] Install Ubuntu

## Phase 2 — Model Layer

- [ ] Write ` + "`scripts/pull_models.sh`" + `
- [ ] Write benchmark_swap.sh that times things

## Phase 2b — Conversation

Goal: pick one.

- [ ] Measure idle power

## Phase 3 — Queue
- [x] Done thing
- [ ] Write the consumer

## Queued jobs — Oct 21

### J0 — Make the queue
- [x] **Run build work from the queue** (branch ` + "`phase-3-x`" + `)
    - text

### D0 — Dashboard bug
- [ ] **Fix the poll** (branch ` + "`phase-6-fix`" + `)
    - Acceptance: tests pass

### D1-D8 — Discord

Context for all D jobs: bot.

- [ ] **D1 — Daemon** (branch ` + "`phase-7-d`" + `)
    - body
- [ ] **D2 — Share**
    - body

### Q1 — Upgrade
- [ ] **Switch the brain**
    - steps
`

func ids(sp []Spec) []string {
	var o []string
	for _, s := range sp {
		o = append(o, s.ID)
	}
	return o
}

func TestPlanOrderAndDeps(t *testing.T) {
	sp := Plan(".", sample, time.Unix(0, 0), []string{"Phase 1"})
	got := strings.Join(ids(sp), ",")
	want := "d0,d1,d2,q1,phase-2-write-benchmark-swap-sh-that-times-things,phase-2b-measure-idle-power,phase-2-write-scripts-pull-models-sh,phase-3-write-the-consumer"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if sp[0].Branch != "phase-6-fix" || len(sp[0].DependsOn) != 0 {
		t.Errorf("d0: %+v", sp[0])
	}
	if sp[1].DependsOn[0] != "d0" || sp[3].DependsOn[0] != "d2" || len(sp[4].DependsOn) != 0 {
		t.Errorf("deps wrong: %v %v %v", sp[1].DependsOn, sp[3].DependsOn, sp[4].DependsOn)
	}
	if !strings.Contains(sp[1].Instructions, "Context for all D jobs") || !strings.Contains(sp[0].Instructions, "Acceptance") {
		t.Errorf("instructions: %q", sp[1].Instructions)
	}
	for i := 1; i < len(sp); i++ {
		if sp[i].Priority >= sp[i-1].Priority {
			t.Fatal("priority must descend in plan order")
		}
	}
	for i, sp := range sp {
		if sp.Class != ClassBackground {
			t.Fatalf("plan job %d (%s): class %q, want background", i, sp.ID, sp.Class)
		}
	}
}

func TestMigrationTwiceAddsNothing(t *testing.T) {
	s := newStore(t)
	add := func() (n int) {
		for _, sp := range Plan(".", sample, time.Now(), []string{"Phase 1"}) {
			if ok, _ := s.Ingest(sp); ok {
				n++
			}
		}
		return n
	}
	if first := add(); first != 8 {
		t.Fatalf("first run added %d", first)
	}
	if second := add(); second != 0 {
		t.Fatalf("second run added %d", second)
	}
	if j, _ := s.Next(); j == nil || j.ID != "d0" {
		t.Fatalf("next = %v", j)
	}
}

// queued-sample mimics the "Queued jobs" index format with doc references
// and dotted IDs including letter suffixes.
const queuedSample = `# Tasks

## Queued jobs — Oct 21

### J0 — Queue
- [ ] **J0.9 — Migration reads index** · ` + "`docs/jobs/j0-queue-work-source.md`" + `
- [ ] **J0.10 — Job rules** · ` + "`docs/jobs/j0-queue-work-source.md`" + `

### D0 — Dashboard
- [ ] **D0.1 — Fix the poll** · ` + "`docs/jobs/d0-dashboard.md`" + `

### E1 — Activity UI
- [ ] **E1.1 — Show status** · ` + "`docs/jobs/e-activity-ui.md`" + `

### R1 — Reliability

Context for all R1 jobs: Discord bot comms.

- [ ] **R1.5a — Ack timeout** · ` + "`docs/jobs/r-discord-reliability.md`" + `
- [ ] **R1.5b — Reconnect** · ` + "`docs/jobs/r-discord-reliability.md`" + `

### Q1 — Upgrade
- [ ] **Q1.1 — Switch model** · ` + "`docs/jobs/q1-qwen38.md`" + `
`

func TestPlanDottedIDs(t *testing.T) {
	sp := Plan(".", queuedSample, time.Unix(0, 0), nil)
	if len(sp) == 0 {
		t.Fatal("expected specs")
	}
	expectedIDs := []string{"j0.9", "j0.10", "d0.1", "e1.1", "r1.5a", "r1.5b", "q1.1"}
	got := ids(sp)
	if len(got) != len(expectedIDs) || !func() bool {
		for i := range got {
			if got[i] != expectedIDs[i] {
				return false
			}
		}
		return true
	}() {
		t.Fatalf("\n got %v\nwant %v", got, expectedIDs)
	}
	// Verify chain dependencies
	for i := 1; i < len(sp); i++ {
		if len(sp[i].DependsOn) != 1 || sp[i].DependsOn[0] != sp[i-1].ID {
			t.Errorf("%s: depends_on = %v, want [%s]", sp[i].ID, sp[i].DependsOn, sp[i-1].ID)
		}
	}
}

func TestPlanDocInstructions(t *testing.T) {
	sp := Plan("../..", queuedSample, time.Unix(0, 0), nil)
	// J0.9 should have instructions from its doc section, not the index line
	j09 := sp[0]
	if !strings.Contains(j09.Instructions, "J0.9") {
		t.Errorf("j0.9 instructions should mention J0.9: %q", j09.Instructions)
	}
	if !strings.Contains(j09.Instructions, "Background:") {
		t.Errorf("j0.9 instructions should have background: %q", j09.Instructions)
	}
	if j09.Doc != "docs/jobs/j0-queue-work-source.md" {
		t.Errorf("j0.9 doc = %q", j09.Doc)
	}
	// R1.5a and R1.5b should have different section content
	r15a := sp[4]
	r15b := sp[5]
	if strings.Contains(r15a.Instructions, "R1.5a") && strings.Contains(r15b.Instructions, "R1.5b") {
		// Good — each has its own section
	} else {
		t.Errorf("R1.5a/R1.5b should have distinct sections")
	}
}

func TestSignalWatcherPausesWhenSlotBusy(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	signaled := false
	signalFile := filepath.Join(t.TempDir(), "signal")
	replySignalFile := filepath.Join(t.TempDir(), "reply-signal")

	r := runner(t, s, `while sleep 0.1; do :; done`)
	r.SignalFile = signalFile
	r.ReplySignalFile = replySignalFile
	r.EngineSlotCheck = func() bool {
		return signaled // slot busy when signaled is true
	}

	// Start the runner in a goroutine — it'll block on the infinite loop.
	// Run() handles Claim internally, so don't pre-claim the job.
	go r.Run(context.Background(), "a", "r1", "local", "")

	// Write the signal file to trigger the watcher
	time.Sleep(200 * time.Millisecond)
	signaled = true // slot busy
	_ = os.WriteFile(signalFile, nil, 0o644)

	// Wait for the watcher to act
	time.Sleep(2 * time.Second)

	// Job should be paused
	got, _ := s.Get("a")
	if got.Status != Paused {
		t.Fatalf("want paused, got %s", got.Status)
	}
	// Reply signal should NOT be written
	if _, err := os.Stat(replySignalFile); err == nil {
		t.Fatal("reply signal should not exist when slot is busy")
	}
}

func TestSignalWatcherDefersReplyWhenSlotFree(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	slotBusy := true
	signalFile := filepath.Join(t.TempDir(), "signal")
	replySignalFile := filepath.Join(t.TempDir(), "reply-signal")

	r := runner(t, s, `while sleep 0.1; do :; done`)
	r.SignalFile = signalFile
	r.ReplySignalFile = replySignalFile
	r.EngineSlotCheck = func() bool {
		return slotBusy
	}

	// Run() handles Claim internally, so don't pre-claim the job.
	go r.Run(context.Background(), "a", "r1", "local", "")

	time.Sleep(200 * time.Millisecond)
	slotBusy = false // slot free
	_ = os.WriteFile(signalFile, nil, 0o644)

	// Wait for the watcher to act
	time.Sleep(2 * time.Second)

	// Reply signal SHOULD be written
	if _, err := os.Stat(replySignalFile); err != nil {
		t.Fatal("reply signal should exist when slot is free")
	}
	// Job should NOT be paused (it keeps running)
	got, _ := s.Get("a")
	if got.Status == Paused {
		t.Fatal("job should not be paused when slot is free")
	}
}

func TestSignalWatcherRestartRequeuesJob(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	restartFile := filepath.Join(t.TempDir(), "restart-signal")
	r := runner(t, s, `while sleep 0.1; do :; done`)
	r.SignalFile = filepath.Join(t.TempDir(), "pause-signal") // starts the watcher
	r.RestartSignalFile = restartFile

	type result struct {
		j   Job
		err error
	}
	done := make(chan result, 1)
	go func() {
		j, err := r.Run(context.Background(), "a", "r1", "local", "")
		done <- result{j, err}
	}()

	// Wait until the round is actually running, so the signal is acted on.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := s.Get("a")
		if got.Status == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached running: %s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = os.WriteFile(restartFile, []byte("!restart"), 0o644)

	out := <-done
	if out.err == nil {
		t.Fatal("want an error from the killed round, got nil")
	}
	if out.j.Status != Ready {
		t.Fatalf("returned job: want ready after restart, got %s", out.j.Status)
	}
	got, _ := s.Get("a")
	if got.Status != Ready {
		t.Fatalf("stored status: want ready after restart, got %s", got.Status)
	}
	// The signal must be consumed, so it cannot kill the next round.
	if _, err := os.Stat(restartFile); !os.IsNotExist(err) {
		t.Fatal("restart-signal should be consumed")
	}
}

func TestStoreRestartIgnoresFinishedJob(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	// A restart that lands when nothing is running is a no-op…
	if err := s.Restart("a"); err != nil {
		t.Fatal(err)
	}
	// …and a late one can never re-run a finished job.
	if err := s.SetStatus("a", State{Status: Done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart("a"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("a")
	if got.Status != Done {
		t.Fatalf("done job must stay done, got %s", got.Status)
	}
}

func TestStoreStop(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	// A stop that lands when nothing is running is refused…
	if err := s.Stop("a"); err == nil {
		t.Fatal("want an error stopping a ready job")
	}
	// …a running job parks…
	if err := s.SetStatus("a", State{Status: Running}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop("a"); err != nil {
		t.Fatal(err)
	}
	// …and a late one is a no-op on a parked job.
	if err := s.Stop("a"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("a")
	if got.Status != Parked {
		t.Fatalf("want parked, got %s", got.Status)
	}
	// A late stop can never park a finished job.
	if err := s.SetStatus("a", State{Status: Done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop("a"); err == nil {
		t.Fatal("want an error stopping a done job")
	}
}

func TestNextSkipsParked(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Priority: 10})
	ingest(t, s, Spec{ID: "b", Priority: 1})
	if err := s.SetStatus("a", State{Status: Parked}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.ID != "b" {
		t.Fatalf("parked jobs are out of the queue; want b, got %v", j)
	}
}

func TestSignalWatcherStopParksJob(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Instructions: "do a"})

	stopFile := filepath.Join(t.TempDir(), "stop-signal")
	r := runner(t, s, `while sleep 0.1; do :; done`)
	r.StopSignalFile = stopFile

	type result struct {
		j   Job
		err error
	}
	done := make(chan result, 1)
	go func() {
		j, err := r.Run(context.Background(), "a", "r1", "local", "")
		done <- result{j, err}
	}()

	// Wait until the round is actually running, so the signal is acted on.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := s.Get("a")
		if got.Status == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached running: %s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = os.WriteFile(stopFile, nil, 0o644)

	out := <-done
	if out.err == nil {
		t.Fatal("want an error from the killed round, got nil")
	}
	if out.j.Status != Parked {
		t.Fatalf("returned job: want parked after stop, got %s", out.j.Status)
	}
	got, _ := s.Get("a")
	if got.Status != Parked {
		t.Fatalf("stored status: want parked after stop, got %s", got.Status)
	}
	// The signal must be consumed, so it cannot park the next round.
	if _, err := os.Stat(stopFile); !os.IsNotExist(err) {
		t.Fatal("stop-signal should be consumed")
	}
}

func TestReadDocSection(t *testing.T) {
	// Read J0.9 section from the actual doc file
	instr, doc := readDocSection("../..", "docs/jobs/j0-queue-work-source.md", "j0.9")
	if instr == "" {
		t.Fatal("expected instructions for j0.9")
	}
	if !strings.Contains(instr, "J0.9") {
		t.Errorf("missing J0.9: %q", instr)
	}
	if doc != "docs/jobs/j0-queue-work-source.md" {
		t.Errorf("doc = %q", doc)
	}
	// Non-existent section returns empty
	empty, _ := readDocSection("../..", "docs/jobs/j0-queue-work-source.md", "zz999")
	if empty != "" {
		t.Errorf("expected empty for missing section, got: %q", empty)
	}
	// Non-existent file returns empty
	empty2, _ := readDocSection("../..", "docs/jobs/no-such-file.md", "j0.9")
	if empty2 != "" {
		t.Errorf("expected empty for missing file, got: %q", empty2)
	}
}

func TestSaveSlotNoOpWhenEngineEmpty(t *testing.T) {
	r := &Runner{}
	err := r.saveSlot("any-id")
	if err != nil {
		t.Fatal("saveSlot should be no-op when EngineURL is empty")
	}
}

func TestRestoreSlotNoOpWhenEngineEmpty(t *testing.T) {
	r := &Runner{}
	err := r.restoreSlot("any-id")
	if err != nil {
		t.Fatal("restoreSlot should be no-op when EngineURL is empty")
	}
}

func TestSaveRestoreSlotHTTP(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())

	// Mock engine that tracks save/restore calls
	saveCalled := false
	restoreCalled := false
	mockEngine := newMockEngine(t, func(action string) int {
		if action == "save" {
			saveCalled = true
		} else if action == "restore" {
			restoreCalled = true
		}
		return 200
	})
	defer mockEngine.Close()

	s := newStore(t)
	ingest(t, s, Spec{ID: "test-job", Title: "Test", Instructions: "do it"})

	r := runner(t, s, `echo ok`)
	r.EngineURL = mockEngine.URL
	r.WorkSlotID = 0

	// Test saveSlot
	err := r.saveSlot("test-job")
	if err != nil {
		t.Fatalf("saveSlot failed: %v", err)
	}
	if !saveCalled {
		t.Fatal("saveSlot should have called the engine")
	}

	// Test restoreSlot
	err = r.restoreSlot("test-job")
	if err != nil {
		t.Fatalf("restoreSlot failed: %v", err)
	}
	if !restoreCalled {
		t.Fatal("restoreSlot should have called the engine")
	}
}

// Signal watcher + saveSlot integration is covered by
// TestSignalWatcherPausesWhenSlotBusy (pre-existing). When EngineURL is set,
// saveSlot is called before Pause in signalWatcher (run.go:241).

// mockEngine is a test HTTP server that simulates the llama.cpp slot API.
type mockEngine struct {
	*http.Server
	URL string
}

// newMockEngine creates an HTTP server that simulates the llama.cpp slot API.
// The handler receives "save" or "restore" as the action string and returns
// the status code it should respond with.
func newMockEngine(t *testing.T, handler func(action string) int) *mockEngine {
	t.Helper()
	s := &http.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/slots/", func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		code := handler(action)
		w.WriteHeader(code)
	})
	s.Handler = mux
	sock, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Addr = sock.Addr().String()
	go s.Serve(sock)
	t.Cleanup(func() { s.Close() })
	return &mockEngine{Server: s, URL: "http://" + sock.Addr().String()}
}

// The iGPU agent exits 0 even when jarvis-photo failed, so a photo round
// that ends without the delivery marker must be recorded failed (re-queue-
// able), not done — otherwise a lost photo is silently closed.
func TestPhotoRoundWithoutDeliveryMarkerFails(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Tags: []string{photo.Tag}})
	r := runner(t, s, `true`)
	j, err := r.Run(context.Background(), "a", "r1", "igpu", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Failed || !strings.Contains(j.Error, "delivery marker") {
		t.Fatalf("want failed photo job, got status=%s error=%q", j.Status, j.Error)
	}
	if got, _ := s.Get("a"); got.Status != Failed {
		t.Fatalf("store status = %s, want failed", got.Status)
	}
}

func TestPhotoRoundWithDeliveryMarkerDone(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A", Tags: []string{photo.Tag}})
	// The round's jarvis-photo generated and delivered: it writes the
	// marker before the agent exits 0.
	script := "mkdir -p " + s.Dir()+"/" + photo.PromptSubdir +
		" && echo ok > " + photo.SentPath(s.Dir(), "a")
	r := runner(t, s, script)
	j, err := r.Run(context.Background(), "a", "r1", "igpu", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Done {
		t.Fatalf("want done, got status=%s error=%q", j.Status, j.Error)
	}
}

// photo-ready is the watcher's batch view: every ready photo job in run
// order — non-photo ready jobs and dependency-blocked photo jobs excluded.
func TestPhotoReady(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "a", Title: "A"})
	ingest(t, s, Spec{ID: "b", Title: "B", Tags: []string{photo.Tag}})
	ingest(t, s, Spec{ID: "c", Title: "C", Tags: []string{photo.Tag}, DependsOn: []string{"a"}})
	ingest(t, s, Spec{ID: "d", Title: "D", Tags: []string{photo.Tag, "x"}})
	ids := func(jobs []Job) []string {
		out := make([]string, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, j.ID)
		}
		return out
	}
	got, err := s.PhotoReady()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "d"}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("PhotoReady = %v, want %v", ids(got), want)
	}
	// When the blocker is done, the blocked photo joins the batch.
	if err := s.setState("a", State{Status: Done}); err != nil {
		t.Fatal(err)
	}
	got, err = s.PhotoReady()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c", "d"}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("PhotoReady after a=done = %v, want %v", ids(got), want)
	}
}
