// Package scripts holds no Go code — only tests that drive the shell
// scripts in this directory with stubbed tools.
//
// agent_run_test.go runs the real scripts/agent-run stage-3 loop against
// stub jarvis-jobs / jarvis-discord binaries and checks how it dispatches
// the owner's !igpu jobs (cmd/jarvis-chat/igpu.go, docs/discord.md):
// which driver each round is started on, and when the loop asks "Who runs
// it?". The loop's PATH holds only the stubs and an allowlist of system
// tools, so the test can never reach a real jarvis-* tool on the machine
// that runs it (no Discord message, no job store, no engine).
package scripts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The stub jarvis-jobs: a queue file ("id<TAB>title" per line, in run
// order), a done file, one show-<id> file per job, and a fullauto flag
// file. Every call that changes or starts something is logged.
const stubJarvisJobs = `#!/bin/bash
cmd="$1"; shift
case "$cmd" in
  nightly) exit 0 ;;
  next)
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      id="${line%%$'\t'*}"
      grep -qx "$id" "$STUB/done" 2>/dev/null && continue
      printf '%s\n' "$line"; exit 0
    done < "$STUB/queue"
    exit 1 ;;
  fullauto)
    if [[ $# -gt 0 ]]; then echo "$1" > "$STUB/fullauto"; echo "jobs fullauto $1" >> "$STUB/log"; fi
    cat "$STUB/fullauto" 2>/dev/null || echo off ;;
  automode|keepgoing) echo off ;;
  show) cat "$STUB/show-$1" 2>/dev/null || exit 1 ;;
  run)
    echo "run $*" >> "$STUB/log"
    # runfail-<id> holds how many more starts of that job fail the way a
    # Redis OOM does: an error on stderr, exit 2, the job left ready.
    if [[ -f "$STUB/runfail-$1" ]] && (( $(cat "$STUB/runfail-$1") > 0 )); then
      echo $(( $(cat "$STUB/runfail-$1") - 1 )) > "$STUB/runfail-$1"
      echo "jarvis-jobs: acquire nobrakes:running: OOM command not allowed when used memory > 'maxmemory'." >&2
      exit 2
    fi
    echo "$1" >> "$STUB/done"; exit 0 ;;
  list)
    while IFS= read -r id; do echo "done $id"; done < "$STUB/done" 2>/dev/null
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      id="${line%%$'\t'*}"
      grep -qx "$id" "$STUB/done" 2>/dev/null || echo "ready $id"
    done < "$STUB/queue" ;;
  *) echo "jobs $cmd $*" >> "$STUB/log" ;;
esac
`

// The stub jarvis-discord: notify is logged; ask logs the question and
// answers with the next line of the answers file ("stop" when it runs
// out, which ends the loop). queue.after-ask<N>, when present, replaces
// the queue as the Nth question is answered — the chat daemon storing a
// job at the moment the owner's message arrives.
const stubJarvisDiscord = `#!/bin/bash
cmd="$1"; shift
case "$cmd" in
  notify) echo "notify $*" >> "$STUB/log" ;;
  ask)
    n=$(( $(cat "$STUB/asks" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$STUB/asks"
    q="${*: -1}"
    echo "ask $(printf '%s' "$q" | tr '\n' ' ')" >> "$STUB/log"
    [[ -f "$STUB/queue.after-ask$n" ]] && mv "$STUB/queue.after-ask$n" "$STUB/queue"
    answer=$(sed -n "${n}p" "$STUB/answers" 2>/dev/null)
    printf '%s\n' "${answer:-stop}" ;;
esac
`

// sleep returns at once. The 60 s full-auto idle poll also turns full auto
// off, so a full-auto scenario ends at a question instead of spinning.
const stubSleep = `#!/bin/bash
echo "sleep $1" >> "$STUB/log"
[[ "$1" == 60 ]] && echo off > "$STUB/fullauto"
exit 0
`

type agentRunJob struct {
	id, title, driver, tags string
}

func (j agentRunJob) queueLine() string { return j.id + "\t" + j.title }

// show mirrors `jarvis-jobs show`: header, status/class/driver/effort, an
// optional tags line, a blank line, the instructions.
func (j agentRunJob) show() string {
	s := j.id + " — " + j.title + "\nstatus: ready\nclass: fix\ndriver: " + j.driver + "\neffort: medium\n"
	if j.tags != "" {
		s += "tags: " + j.tags + "\n"
	}
	return s + "\ndo the thing\n"
}

type agentRunScenario struct {
	jobs     []agentRunJob         // every job the loop may `show`
	queue    []agentRunJob         // the queue at start, in run order
	afterAsk map[int][]agentRunJob // the queue once question N is answered
	answers  []string              // checkpoint answers, in order ("stop" after)
	fullAuto bool
	runFail  map[string]int // job id -> how many of its starts fail (exit 2, still ready)
}

// runAgentRun runs scripts/agent-run in stage 3 against the stubs and
// returns the stub log, one line per call.
func runAgentRun(t *testing.T, sc agentRunScenario) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs("agent-run")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	bin, stub, conf := filepath.Join(root, "bin"), filepath.Join(root, "stub"), filepath.Join(root, "conf")
	for _, d := range []string{bin, stub, conf} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// The loop's whole PATH: the stubs, plus the system tools the script
	// itself uses. Anything else — every real jarvis-* tool — is not found.
	for _, tool := range []string{"bash", "cat", "sed", "tr", "grep", "awk", "date", "rm", "mv", "head", "tail", "cut", "hostname", "whoami"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "jarvis-jobs"), stubJarvisJobs, 0o755)
	write(filepath.Join(bin, "jarvis-discord"), stubJarvisDiscord, 0o755)
	write(filepath.Join(bin, "sleep"), stubSleep, 0o755)
	// systemctl exit 0 = "jarvis-chat is active": the loop starts no
	// Discord watcher of its own.
	for _, ok := range []string{"systemctl", "ghapp-token", "gh", "jarvis-homeai"} {
		write(filepath.Join(bin, ok), "#!/bin/bash\nexit 0\n", 0o755)
	}

	lines := func(jobs []agentRunJob) string {
		var b strings.Builder
		for _, j := range jobs {
			b.WriteString(j.queueLine() + "\n")
		}
		return b.String()
	}
	for _, j := range sc.jobs {
		write(filepath.Join(stub, "show-"+j.id), j.show(), 0o644)
	}
	write(filepath.Join(stub, "queue"), lines(sc.queue), 0o644)
	for n, q := range sc.afterAsk {
		write(filepath.Join(stub, "queue.after-ask"+string(rune('0'+n))), lines(q), 0o644)
	}
	write(filepath.Join(stub, "answers"), strings.Join(sc.answers, "\n")+"\n", 0o644)
	write(filepath.Join(stub, "log"), "", 0o644)
	write(filepath.Join(stub, "done"), "", 0o644)
	if sc.fullAuto {
		write(filepath.Join(stub, "fullauto"), "on\n", 0o644)
	}
	for id, n := range sc.runFail {
		write(filepath.Join(stub, "runfail-"+id), strconv.Itoa(n)+"\n", 0o644)
	}
	write(filepath.Join(conf, "stage"), "3\n", 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "JARVIS_CONF=" + conf, "STUB=" + stub}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("agent-run did not stop (the loop never reached a question answered with stop):\n%s", out)
	}
	if err != nil {
		t.Fatalf("agent-run: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(stub, "log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func withPrefix(log []string, prefix string) []string {
	var out []string
	for _, l := range log {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func indexOf(log []string, prefix string) int {
	for i, l := range log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

var (
	igpuDirectJob = agentRunJob{id: "chat-1", title: "igpu: restart the brain", driver: "igpu", tags: "igpu-direct"}
	localJob      = agentRunJob{id: "j1", title: "a local job", driver: "local"}
)

// Supervised loop: an !igpu job is dispatched on the iGPU model without the
// "Who runs it?" question, and not as an auto round.
func TestAgentRunDispatchesIGPUDirectJobWithoutAsking(t *testing.T) {
	log := runAgentRun(t, agentRunScenario{
		jobs:  []agentRunJob{igpuDirectJob},
		queue: []agentRunJob{igpuDirectJob},
	})
	runs := withPrefix(log, "run ")
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "run chat-1 -driver igpu ") {
		t.Fatalf("runs = %q, want one round of chat-1 on the igpu driver\nlog:\n%s", runs, strings.Join(log, "\n"))
	}
	if strings.Contains(runs[0], "-auto") {
		t.Errorf("a supervised round must not take the auto marker: %q", runs[0])
	}
	if ask := indexOf(log, "ask "); ask >= 0 && ask < indexOf(log, "run ") {
		t.Errorf("the loop asked before running the !igpu job:\n%s", strings.Join(log, "\n"))
	}
	if indexOf(log, "notify iGPU job igpu: restart the brain (chat-1)") < 0 {
		t.Errorf("no dispatch note for the !igpu job:\n%s", strings.Join(log, "\n"))
	}
	// The only question is the one after the round: nothing left to run.
	asks := withPrefix(log, "ask ")
	if len(asks) != 1 || !strings.Contains(asks[0], "No ready jobs in the queue") {
		t.Errorf("asks = %q, want one empty-queue question after the round", asks)
	}
}

// !igpu typed while "Who runs it?" is open is a chat command, not round
// instructions: the job named in the question is NOT started on the local
// model; the loop re-reads the queue and runs the freshly queued !igpu job.
func TestAgentRunTreatsIGPUAnswerAsCommand(t *testing.T) {
	for _, answer := range []string{"!igpu restart the brain", "!IGPU restart the brain", "!igpu\tjarvis-swap in", "!igpu"} {
		log := runAgentRun(t, agentRunScenario{
			jobs:     []agentRunJob{igpuDirectJob, localJob},
			queue:    []agentRunJob{localJob},
			afterAsk: map[int][]agentRunJob{1: {igpuDirectJob, localJob}},
			answers:  []string{answer},
		})
		runs := withPrefix(log, "run ")
		if len(runs) != 1 || !strings.HasPrefix(runs[0], "run chat-1 -driver igpu ") {
			t.Errorf("answer %q: runs = %q, want only chat-1 on the igpu driver\nlog:\n%s", answer, runs, strings.Join(log, "\n"))
		}
		asks := withPrefix(log, "ask ")
		if len(asks) != 2 || !strings.Contains(asks[0], "Next: a local job (j1)") || !strings.Contains(asks[1], "Next: a local job (j1)") {
			t.Errorf("answer %q: asks = %q, want the local job named before and after the !igpu round", answer, asks)
		}
	}
}

// When the chat daemon queued nothing (it is down, or runs an older
// build), the !igpu answer still starts no round, and the next question
// says so.
func TestAgentRunIGPUAnswerWithNoJobQueued(t *testing.T) {
	log := runAgentRun(t, agentRunScenario{
		jobs:    []agentRunJob{localJob},
		queue:   []agentRunJob{localJob},
		answers: []string{"!igpu restart the brain"},
	})
	if runs := withPrefix(log, "run "); len(runs) != 0 {
		t.Fatalf("runs = %q, want none", runs)
	}
	asks := withPrefix(log, "ask ")
	if len(asks) != 2 || !strings.Contains(asks[1], "send !igpu again") {
		t.Errorf("asks = %q, want the second question to say the !igpu job did not show up", asks)
	}
}

// The checkpoint is unchanged for everything else: an ordinary job is
// asked about, a typed answer is still a note for a local round (also one
// that merely starts with the letters "!igpu"), and the buttons pick the
// driver.
func TestAgentRunOrdinaryJobsStillAsk(t *testing.T) {
	for _, c := range []struct{ answer, want string }{
		{"do it carefully", "run j1 -driver local -round "},
		{"!igpus are slow, use the brain", "run j1 -driver local -round "},
		{"iGPU model", "run j1 -driver igpu -round "},
		{"Sonnet", "run j1 -driver sonnet -round "},
	} {
		log := runAgentRun(t, agentRunScenario{
			jobs:    []agentRunJob{localJob},
			queue:   []agentRunJob{localJob},
			answers: []string{c.answer},
		})
		runs := withPrefix(log, "run ")
		if len(runs) != 1 || !strings.HasPrefix(runs[0], c.want) {
			t.Errorf("answer %q: runs = %q, want prefix %q", c.answer, runs, c.want)
			continue
		}
		if ask := indexOf(log, "ask "); ask < 0 || ask > indexOf(log, "run ") {
			t.Errorf("answer %q: an ordinary job must be asked about before it runs", c.answer)
		}
		if c.want == "run j1 -driver local -round " && !strings.Contains(runs[0], "-note "+c.answer) {
			t.Errorf("answer %q: the typed text must reach the round as its note: %q", c.answer, runs[0])
		}
	}
}

// Photo jobs keep their own no-question dispatch (the branch the !igpu
// case sits next to).
func TestAgentRunPhotoJobStillDispatchesAsCode(t *testing.T) {
	photo := agentRunJob{id: "p1", title: "photo: a red panda", driver: "igpu", tags: "photo"}
	log := runAgentRun(t, agentRunScenario{jobs: []agentRunJob{photo}, queue: []agentRunJob{photo}})
	runs := withPrefix(log, "run ")
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "run p1 -driver igpu ") {
		t.Fatalf("runs = %q", runs)
	}
	if indexOf(log, "notify Photo job photo: a red panda (p1)") < 0 || indexOf(log, "notify iGPU job") >= 0 {
		t.Errorf("a photo job must take the photo dispatch, not the !igpu one:\n%s", strings.Join(log, "\n"))
	}
}

// Full auto honors the job's spec driver, so an !igpu job runs on the
// iGPU model there too — as an auto round, with no question.
func TestAgentRunFullAutoRunsIGPUJobOnItsSpecDriver(t *testing.T) {
	log := runAgentRun(t, agentRunScenario{
		jobs:     []agentRunJob{igpuDirectJob},
		queue:    []agentRunJob{igpuDirectJob},
		fullAuto: true,
	})
	runs := withPrefix(log, "run ")
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "run chat-1 -driver igpu ") || !strings.HasSuffix(runs[0], "-auto") {
		t.Fatalf("runs = %q, want chat-1 on the igpu driver as an auto round\nlog:\n%s", runs, strings.Join(log, "\n"))
	}
	if indexOf(log, "notify Full auto: running igpu: restart the brain (chat-1) on igpu.") < 0 {
		t.Errorf("no full-auto dispatch note:\n%s", strings.Join(log, "\n"))
	}
}

// Full auto, a job that cannot start (Redis OOM refusing the auto-run
// marker, 2026-11-20): the loop must not re-dispatch it at once and post
// "Full auto: running ..." on every iteration. It posts the running note
// once, the failure once with its reason, and backs off between retries.
func TestAgentRunFullAutoBacksOffWhenAJobCannotStart(t *testing.T) {
	log := runAgentRun(t, agentRunScenario{
		jobs:     []agentRunJob{localJob},
		queue:    []agentRunJob{localJob},
		fullAuto: true,
		runFail:  map[string]int{"j1": 5},
	})
	all := strings.Join(log, "\n")
	// Backoff 30 s, then 60 s — the stub's 60 s sleep turns full auto off,
	// so the scenario ends at a question after two failed starts.
	if runs := withPrefix(log, "run j1 "); len(runs) != 2 {
		t.Fatalf("runs = %q, want 2 (one per backoff step)\n%s", runs, all)
	}
	if n := len(withPrefix(log, "notify Full auto: running a local job (j1)")); n != 1 {
		t.Errorf("running note posted %d times, want once:\n%s", n, all)
	}
	fails := withPrefix(log, "notify Full auto: couldn't start a local job (j1)")
	if len(fails) != 1 || !strings.Contains(fails[0], "OOM command not allowed") {
		t.Errorf("failure notes = %q, want one carrying the run's error:\n%s", fails, all)
	}
	if indexOf(log, "sleep 30") < 0 || indexOf(log, "sleep 60") < indexOf(log, "sleep 30") {
		t.Errorf("want a 30 s then a 60 s backoff:\n%s", all)
	}
}

// Once the job starts after failed starts, the owner hears it ran.
func TestAgentRunFullAutoReportsRecoveryAfterFailedStart(t *testing.T) {
	log := runAgentRun(t, agentRunScenario{
		jobs:     []agentRunJob{localJob},
		queue:    []agentRunJob{localJob},
		fullAuto: true,
		runFail:  map[string]int{"j1": 1},
	})
	all := strings.Join(log, "\n")
	if runs := withPrefix(log, "run j1 "); len(runs) != 2 {
		t.Fatalf("runs = %q, want a failed start then the real round\n%s", runs, all)
	}
	if n := len(withPrefix(log, "notify Full auto: running a local job (j1)")); n != 1 {
		t.Errorf("running note posted %d times, want once:\n%s", n, all)
	}
	if indexOf(log, "notify Full auto: a local job (j1) ran after 1 failed start(s) (exit 0).") < 0 {
		t.Errorf("no recovery note:\n%s", all)
	}
}
