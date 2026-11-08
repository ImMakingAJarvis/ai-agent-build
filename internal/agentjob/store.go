// Package agentjob stores build work ("agent jobs") that the agent-run loop
// runs one at a time with a Claude Code session. The worker only ingests an
// agent job from RabbitMQ (writes it here, then acks), so no message is held
// unacked for the length of a round.
package agentjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

// Job statuses.
const (
	Ready   = "ready"
	Claimed = "claimed"
	Running = "running"
	Paused  = "paused"
	Done    = "done"
	Failed  = "failed"
	Skipped = "skipped"
	// Parked: the job is defined but not in the active queue. Nightly-tagged
	// jobs (TagNightly) are stored parked when they land in the store and
	// stay parked until the NoBrakes nightly gate (internal/nobrakes.
	// AutoEnqueue) moves them to ready. Next never returns parked jobs.
	Parked = "parked"
)

// Job classes, highest priority first. A job's class decides where it sits
// in the queue: fix jobs always run before build jobs, which always run
// before background (TASKS.md backlog) jobs — regardless of numeric
// priority, which only orders jobs within the same class.
const (
	ClassFix        = "fix"
	ClassBuild      = "build"
	ClassBackground = "background"
)

// EffectiveClass normalizes a job's class. An empty class (jobs from before
// the field existed) falls back to its source: work published from
// TASKS.md (migrate) is background, everything else the owner asked for is
// build. Unknown classes count as build so new kinds of user-requested
// work still beat the backlog.
func EffectiveClass(class, source string) string {
	switch class {
	case ClassFix, ClassBuild, ClassBackground:
		return class
	case "":
		if source == "migrate" {
			return ClassBackground
		}
		return ClassBuild
	default:
		return ClassBuild
	}
}

// ClassRank orders classes for sorting under the default band order: lower
// runs first (fix, build, background). The runtime order can differ — see
// Store.Rank and Store.BandOrder; List sorts by the runtime order.
func ClassRank(class, source string) int {
	switch EffectiveClass(class, source) {
	case ClassFix:
		return 0
	case ClassBuild:
		return 1
	default:
		return 2
	}
}

// Spec is the immutable job definition (job.json).
type Spec struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Instructions    string    `json:"instructions"`
	Doc             string    `json:"doc,omitempty"` // path to job doc section
	Branch          string    `json:"branch,omitempty"`
	DependsOn       []string  `json:"depends_on,omitempty"`
	Driver          string    `json:"driver,omitempty"` // local (default) or sonnet
	Class           string    `json:"class,omitempty"`  // fix, build, background (see Class constants)
	Priority        int       `json:"priority"`         // higher runs first within the class
	CreatedAt       time.Time `json:"created_at"`
	Source          string    `json:"source,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"` // off, low, medium, high, xhigh, max
	// DiscordChannelID / DiscordMessageID identify the owner message that
	// scheduled this job. The worker reacts to that message on pickup.
	DiscordChannelID string `json:"discord_channel_id,omitempty"`
	DiscordMessageID string `json:"discord_message_id,omitempty"`

	// KanbanTaskID links the job to the kanban task it was created from
	// (NoBrakes 4/6). When set, the nightly gate reads the task's nightly
	// tag on the board as the canonical auto-eligibility source.
	KanbanTaskID string `json:"kanban_task_id,omitempty"`

	// Tags is an open set of job-level markers (job.json "tags"). Tags with
	// built-in semantics: TagNightly (NoBrakes nightly gate), TagPhoto
	// (brain-swap photo window).
	Tags []string `json:"tags,omitempty"`
}

// TagNightly marks a job for NoBrakes autonomous nightly execution. A
// nightly-tagged job is stored parked (out of the active queue) and is
// auto-enqueued — moved to ready — by the nightly gate when the Redis
// auto_mode flag is on. NoBrakes 4/6 makes the kanban task's nightly tag
// the canonical source; this job-level tag remains the fallback for
// parked jobs with no kanban link.
const TagNightly = "nightly"

// TagPhoto marks a job as photo work: it needs the 4090 without the brain
// (the art/bench services wait for 12 GB free before generating, see
// docs/brain-swap.md). The queue watcher (scripts/agent-run) opens a
// brain-swap window for the batch of ready photo-tagged jobs before the
// first one runs and swaps the brain back in when the batch drains. Set
// with `jarvis-jobs add -tags photo`, and by image-queue ingestion.
const TagPhoto = "photo"

// HasTag reports whether the spec carries tag.
func (sp Spec) HasTag(tag string) bool {
	for _, t := range sp.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// State is the mutable record (state.json).
type State struct {
	Status     string `json:"status"`
	RanWith    string `json:"ran_with,omitempty"` // driver of the current/last run
	Model      string `json:"model,omitempty"`
	Round      string `json:"round,omitempty"`
	PID        int    `json:"pid,omitempty"` // owner of a claimed/running job
	ClaimedAt  string `json:"claimed_at,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Duration   string `json:"duration,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Error      string `json:"error,omitempty"`
	// Power (local runs): energy sampled from the GPU while the job ran,
	// and its electricity cost at the configured $/kWh rate
	// (internal/power). Cumulative across all attempts of the job.
	Kwh       float64 `json:"kwh,omitempty"`
	PowerCost float64 `json:"power_cost,omitempty"`
}

// Job is a spec plus its state.
type Job struct {
	Spec
	State
}

// Store keeps jobs under <dir>/jobs/<id>/.
type Store struct{ dir string }

// DataDir returns JARVIS_DATA_DIR or ~/.local/share/jarvis.
func DataDir() string {
	if d := os.Getenv("JARVIS_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return filepath.Join(home, ".local", "share", "jarvis")
}

// Open creates the store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store's data directory (jobs.jsonl lives here too).
func (s *Store) Dir() string { return s.dir }

// JobDir is where a job's files (job.json, state.json, transcript.txt,
// output.txt) live.
func (s *Store) JobDir(id string) string { return filepath.Join(s.dir, "jobs", id) }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Ingest stores a new job. A job tagged nightly (TagNightly) is stored
// parked — held out of the active queue until the NoBrakes nightly gate
// (internal/nobrakes.AutoEnqueue) enqueues it; every other job is stored
// ready, as before. It is idempotent: if the job already exists (a
// redelivered message, or a second migration run) nothing changes and
// added is false.
func (s *Store) Ingest(sp Spec) (added bool, err error) {
	if sp.ID == "" || filepath.Base(sp.ID) != sp.ID {
		return false, fmt.Errorf("agentjob: bad job id %q", sp.ID)
	}
	if err := os.Mkdir(s.JobDir(sp.ID), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if sp.CreatedAt.IsZero() {
		sp.CreatedAt = time.Now().UTC()
	}
	status := Ready
	if sp.HasTag(TagNightly) {
		status = Parked
	}
	if err := writeJSON(filepath.Join(s.JobDir(sp.ID), "state.json"), State{Status: status}); err != nil {
		return false, err
	}
	// job.json is written last: a job without it is not yet visible.
	return true, writeJSON(filepath.Join(s.JobDir(sp.ID), "job.json"), sp)
}

// Refresh re-triggers a finished job with new instructions (B5's
// repeated re-plan: the epic's comments changed, so the stored
// instructions must be replaced, not just re-queued). It replaces
// job.json with sp and puts the job back to ready. It refuses jobs
// that are not finished (done/failed/skipped): re-writing the
// instructions under an in-flight runner would be wrong, and the
// caller is expected to have checked the status anyway.
func (s *Store) Refresh(sp Spec) error {
	j, err := s.Get(sp.ID)
	if err != nil {
		return err
	}
	switch j.Status {
	case Done, Failed, Skipped:
	default:
		return fmt.Errorf("agentjob: job %s is %s, not finished", sp.ID, j.Status)
	}
	if sp.CreatedAt.IsZero() {
		sp.CreatedAt = time.Now().UTC()
	}
	if err := writeJSON(filepath.Join(s.JobDir(sp.ID), "job.json"), sp); err != nil {
		return err
	}
	// A re-triggered nightly job waits for the next nightly gate, not for
	// the queue.
	j.Status = Ready
	if sp.HasTag(TagNightly) {
		j.Status = Parked
	}
	return s.setState(sp.ID, j.State)
}

// Get loads one job.
func (s *Store) Get(id string) (Job, error) {
	var j Job
	b, err := os.ReadFile(filepath.Join(s.JobDir(id), "job.json"))
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j.Spec); err != nil {
		return j, err
	}
	b, err = os.ReadFile(filepath.Join(s.JobDir(id), "state.json"))
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(b, &j.State)
}

func (s *Store) setState(id string, st State) error {
	return writeJSON(filepath.Join(s.JobDir(id), "state.json"), st)
}

// List returns all jobs in queue order: band first (in the current runtime
// band order, default fix, then build, then background — Store.BandOrder),
// then priority desc within the band, oldest first, then ID.
func (s *Store) List() ([]Job, error) {
	rank, err := s.bandRank()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(s.dir, "jobs"))
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if j, err := s.Get(e.Name()); err == nil {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(a, b int) bool {
		x, y := jobs[a], jobs[b]
		if rx, ry := rank[EffectiveClass(x.Class, x.Source)], rank[EffectiveClass(y.Class, y.Source)]; rx != ry {
			return rx < ry
		}
		if x.Priority != y.Priority {
			return x.Priority > y.Priority
		}
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return x.CreatedAt.Before(y.CreatedAt)
		}
		return x.ID < y.ID
	})
	return jobs, nil
}

// SetOrder updates a job's class and within-class priority (higher priority
// runs first within the class). It rewrites the job's spec (job.json) and
// leaves its state untouched. This is the persistence behind the dashboard's
// drag-to-reorder queue: dragging reorders a band by rewriting priorities,
// and dropping a job in another band reclassifies it.
func (s *Store) SetOrder(id, class string, priority int) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	j.Class = class
	j.Priority = priority
	return writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec)
}

// SetDriver rewrites a job's driver (job.json "driver") without touching
// its state. The brain-swap window uses it to flip queued photo jobs to
// the iGPU driver while the brain is off (docs/brain-swap.md).
func (s *Store) SetDriver(id, driver string) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	switch driver {
	case "local", "igpu", "sonnet", "superagent":
	default:
		return fmt.Errorf("agentjob: unknown driver %q", driver)
	}
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	j.Driver = driver
	return writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec)
}

// ApplyOrder rewrites the class and priority of each listed job so that,
// within each class band, jobs run in the given top-to-bottom order (the
// first id runs first). The band order itself (fix, build, background) is
// fixed and is not affected by the map's iteration order.
func (s *Store) ApplyOrder(orders map[string][]string) error {
	for class, ids := range orders {
		if class != ClassFix && class != ClassBuild && class != ClassBackground {
			return fmt.Errorf("agentjob: unknown class %q", class)
		}
		for i, id := range ids {
			// Top (i=0) gets the highest priority so it runs first.
			if err := s.SetOrder(id, class, len(ids)-i); err != nil {
				return err
			}
		}
	}
	return nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Edit updates a ready job's title and/or instructions — the text of the
// work. A non-empty field is replaced; an empty field leaves the stored
// text unchanged. It refuses jobs that are not ready: the queue UI edits
// queued work only, and re-writing the text under an in-flight or finished
// job would be wrong (finished jobs are re-triggered with Refresh instead).
func (s *Store) Edit(id, title, instructions string) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	if title == "" && instructions == "" {
		return fmt.Errorf("agentjob: nothing to edit for job %s", id)
	}
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Ready {
		return fmt.Errorf("agentjob: job %s is %s, not ready", id, j.Status)
	}
	if title != "" {
		j.Title = title
	}
	if instructions != "" {
		j.Instructions = instructions
	}
	return writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec)
}

// Recover puts claimed/running jobs whose owner process is gone (crash or
// reboot) back to ready. A job whose owner is alive is left alone, so it is
// never run twice at once.
func (s *Store) Recover() error {
	jobs, err := s.List()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if (j.Status == Claimed || j.Status == Running) && !alive(j.PID) {
			if err := s.setState(j.ID, State{Status: Ready}); err != nil {
				return err
			}
		}
		// Paused jobs whose owner is gone are resumed to ready.
		if j.Status == Paused && !alive(j.PID) {
			if err := s.setState(j.ID, State{Status: Ready}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Next returns the next ready job whose dependencies are all done, or nil.
// Next returns the next runnable job: ready, dependencies done.
func (s *Store) Next() (*Job, error) { return s.NextExcluding(nil) }

// NextExcluding is Next, but never returns a job whose id is in exclude.
// The CLI uses it to skip jobs whose driver's engine is down (docs/brain-
// swap.md): while the brain is off for a photo window, local jobs stay
// ready and the worker idles on them instead of burning a dead round.
func (s *Store) NextExcluding(exclude map[string]bool) (*Job, error) {
	if err := s.Recover(); err != nil {
		return nil, err
	}
	jobs, err := s.List()
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	for _, j := range jobs {
		status[j.ID] = j.Status
	}
	for _, j := range jobs {
		if j.Status != Ready || exclude[j.ID] {
			continue
		}
		ok := true
		for _, d := range j.DependsOn {
			if status[d] != Done {
				ok = false
				break
			}
		}
		if ok {
			j := j
			return &j, nil
		}
	}
	return nil, nil
}

// PhotoReady returns every ready photo-tagged job whose dependencies are all
// done, in List() (run) order. This is the batch the agent-run watcher
// brackets with ONE brain-swap window: it opens the window for the batch,
// runs each photo job's iGPU round, and closes it when the batch drains.
// jarvis-jobs photo-ready prints their ids comma-separated.
func (s *Store) PhotoReady() ([]Job, error) {
	if err := s.Recover(); err != nil {
		return nil, err
	}
	jobs, err := s.List()
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	for _, j := range jobs {
		status[j.ID] = j.Status
	}
	var ready []Job
	for _, j := range jobs {
		if j.Status != Ready || !j.HasTag(photo.Tag) {
			continue
		}
		ok := true
		for _, d := range j.DependsOn {
			if status[d] != Done {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, j)
		}
	}
	return ready, nil
}

// Claim marks a ready job as claimed by this process. It fails if the job
// is not ready, which is what stops two runners taking the same job.
func (s *Store) Claim(id, round, driver string) (Job, error) {
	lock, err := os.OpenFile(filepath.Join(s.JobDir(id), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Job{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	j, err := s.Get(id)
	if err != nil {
		return j, err
	}
	if (j.Status == Claimed || j.Status == Running) && !alive(j.PID) {
		j.Status = Ready
	}
	if j.Status != Ready {
		return j, fmt.Errorf("job %s is %s, not ready", id, j.Status)
	}
	j.State = State{Status: Claimed, RanWith: driver, Round: round, PID: os.Getpid(),
		ClaimedAt: time.Now().UTC().Format(time.RFC3339)}
	return j, s.setState(id, j.State)
}

// SetStatus overwrites a job's state (skip, requeue, results).
func (s *Store) SetStatus(id string, st State) error { return s.setState(id, st) }

// Pause transitions a running job to paused. It is idempotent: if the job
// is already paused nothing changes and nil is returned. The paused job can
// later be resumed back to ready with SetStatus(Ready).
func (s *Store) Pause(id string) error {
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status == Paused {
		return nil
	}
	if j.Status != Running {
		return fmt.Errorf("job %s is %s, not running", id, j.Status)
	}
	j.Status = Paused
	return s.setState(id, j.State)
}

// Stop parks a running job: the round is killed and the job is taken out
// of the active queue, so the agent-run loop skips to the next work. It is
// the counterpart of Pause, but the job is not left in the queue; re-queue
// it later with SetStatus(Ready). Like Pause, it is a no-op when the job
// is already parked, and it refuses a job that is not running: a signal
// that lands after a clean finish must not park a done or skipped job.
func (s *Store) Stop(id string) error {
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status == Parked {
		return nil
	}
	if j.Status != Running {
		return fmt.Errorf("job %s is %s, not running", id, j.Status)
	}
	j.Status = Parked
	return s.setState(id, j.State)
}

// Restart puts a running job back to ready for a fresh run (the "!restart"
// chat command kills the round and re-queues it). It is the counterpart of
// Pause, but a no-op when the job is not running: a signal that lands after
// a clean finish must not re-run a done or skipped job.
func (s *Store) Restart(id string) error {
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Running {
		return nil
	}
	j.Status = Ready
	return s.setState(id, j.State)
}

// WriteFile stores a named artifact (output.txt, transcript.txt) for a job.
func (s *Store) WriteFile(id, name string, data []byte) error {
	if err := os.MkdirAll(s.JobDir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.JobDir(id), name), data, 0o644)
}

// ReadFile loads a named artifact from a job directory. It returns
// os.ErrNotExist when the file is missing.
func (s *Store) ReadFile(id, name string) ([]byte, error) {
	if !safeJobID(id) || filepath.Base(name) != name {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(s.JobDir(id), name))
}

// ValidID is true when id is a single path segment (no traversal).
func ValidID(id string) bool {
	return id != "" && filepath.Base(id) == id && id != "." && id != ".."
}

func safeJobID(id string) bool { return ValidID(id) }
