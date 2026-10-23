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
)

// Spec is the immutable job definition (job.json).
type Spec struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Instructions string    `json:"instructions"`
	Doc          string    `json:"doc,omitempty"`        // path to job doc section
	Branch       string    `json:"branch,omitempty"`
	DependsOn    []string  `json:"depends_on,omitempty"`
	Driver       string    `json:"driver,omitempty"` // local (default) or sonnet
	Priority     int       `json:"priority"`
	CreatedAt    time.Time `json:"created_at"`
	Source       string    `json:"source,omitempty"`
	// DiscordChannelID / DiscordMessageID identify the owner message that
	// scheduled this job. The worker reacts to that message on pickup.
	DiscordChannelID string `json:"discord_channel_id,omitempty"`
	DiscordMessageID string `json:"discord_message_id,omitempty"`
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

// Ingest stores a new job as ready. It is idempotent: if the job already
// exists (a redelivered message, or a second migration run) nothing changes
// and added is false.
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
	if err := writeJSON(filepath.Join(s.JobDir(sp.ID), "state.json"), State{Status: Ready}); err != nil {
		return false, err
	}
	// job.json is written last: a job without it is not yet visible.
	return true, writeJSON(filepath.Join(s.JobDir(sp.ID), "job.json"), sp)
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

// List returns all jobs in queue order (priority desc, oldest first, ID).
func (s *Store) List() ([]Job, error) {
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

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
func (s *Store) Next() (*Job, error) {
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
		if j.Status != Ready {
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

// WriteFile stores a named artifact (output.txt, transcript.txt) for a job.
func (s *Store) WriteFile(id, name string, data []byte) error {
	if err := os.MkdirAll(s.JobDir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.JobDir(id), name), data, 0o644)
}
