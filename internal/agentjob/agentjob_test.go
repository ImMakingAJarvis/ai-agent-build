package agentjob

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
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

func TestPromptHasRulesJobAndNote(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir()) // don't drain the real inbox
	r := &Runner{Brief: func(string) string { return "RULES" }}
	p := r.Prompt(Job{Spec: Spec{ID: "d0", Title: "T", Instructions: "TEXT", Branch: "br"}}, "local", "NOTE")
	for _, w := range []string{"RULES", "d0", "TEXT", "br", "NOTE"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q", w)
		}
	}
}

func TestPromptIncludesIdleOwnerMessages(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	if _, err := inbox.Send("hello from the owner"); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Brief: func(string) string { return "RULES" }}
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
