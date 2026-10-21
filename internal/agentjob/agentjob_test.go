package agentjob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Command:  func(driver, prompt string) *exec.Cmd { return exec.Command("sh", "-c", script) },
		PRLookup: func(string) string { return "" },
		Brief:    func(string) string { return "RULES" },
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

func TestPromptHasRulesJobAndNote(t *testing.T) {
	r := &Runner{Brief: func(string) string { return "RULES" }}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT", Branch: "br"}}, "local", "NOTE")
	for _, w := range []string{"RULES", "d0", "TEXT", "br", "NOTE"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q", w)
		}
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
