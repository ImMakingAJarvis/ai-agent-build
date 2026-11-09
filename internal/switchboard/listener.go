// Package switchboard owns listeners — durable records of the form "when a
// message lands on queue Q, do action A". It is the service half of the
// event-driven brain's wait/wake loop (plan task 14, docs/event-driven.md,
// "Parking a round on a result"): a brain round yields on a set of
// correlation ids (agentjob.Store.Yield), the switchboard listens on the
// reply queue, and when a result lands it records it (Store.RecordResult)
// and wakes the job (Store.Wake) — the woken round then consumes and acks
// the result itself.
//
// Listener store: JSON, one file per listener under
// ~/.local/share/jarvis/listeners/ (the same root as the agentjob store).
// The choice (the plan offers Redis or JSON, pick one): JSON, because the
// switchboard already reads and writes the file-backed agentjob store to
// record results and wake jobs, so one data plane covers both. It keeps the
// same atomic-write durability class as job state (tmp + rename), a listener
// survives a switchboard restart (plan invariant 3) with no second
// dependency or failure domain, and a listener's lifetime is tied to the
// job's (both live under the jarvis data root). A listener is created by
// `jarvis-mq listen` (plan task 15) and removed when the job finishes.
package switchboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Action is what a listener does when a matching message lands on its queue.
const (
	// ActionWake peeks the result (does not remove it — the woken round
	// consumes and acks it), records it against the job, and wakes the job.
	ActionWake = "wake"
	// ActionForward republishes the message to TargetQueue and acks it after
	// the publish is confirmed.
	ActionForward = "forward"
	// ActionNotify sends a Discord notification and acks the message.
	ActionNotify = "notify"
)

// TimeoutAction is what happens when a wake listener's deadline passes with
// the correlation still unarrived.
const (
	// TimeoutWake wakes the job with status=timeout (the default).
	TimeoutWake = "wake"
	// TimeoutNotify sends a Discord notification instead of waking.
	TimeoutNotify = "notify"
)

// Listener is one durable "when a message lands on Queue, do Action" record.
// It is stored as <dir>/<id>.json.
type Listener struct {
	ID string `json:"id"`
	// Queue is the queue the listener watches, e.g. "flow.<job-id>".
	Queue string `json:"queue"`
	// Action is wake, forward, or notify.
	Action string `json:"action"`
	// JobID: for a wake listener, the agentjob to wake.
	JobID string `json:"job_id,omitempty"`
	// TargetQueue: for a forward listener, the queue to republish to.
	TargetQueue string `json:"target_queue,omitempty"`
	// Text: for a notify listener (and timeout notify), the Discord text.
	Text string `json:"text,omitempty"`
	// Timeout: 0 = no timeout. For a wake listener this is the request-time
	// hint for how long to wait; the authoritative deadline is the job's
	// WaitUntil (set by Yield), which the timeout monitor consults first.
	Timeout time.Duration `json:"timeout,omitempty"`
	// TimeoutAction: what to do when Timeout elapses (wake, the default, or
	// notify).
	TimeoutAction string `json:"timeout_action,omitempty"`
	// CorrFilter: when non-empty, only a message whose correlation id equals
	// this triggers the listener. Empty = any message on the queue.
	CorrFilter string `json:"corr_filter,omitempty"`
	// CreatedAt: when the listener was created.
	CreatedAt time.Time `json:"created_at"`
}

// valid reports whether the listener is well-formed enough to store.
func (l Listener) valid() error {
	if l.ID == "" || filepath.Base(l.ID) != l.ID {
		return fmt.Errorf("switchboard: bad listener id %q", l.ID)
	}
	if l.Queue == "" {
		return fmt.Errorf("switchboard: listener %s has no queue", l.ID)
	}
	switch l.Action {
	case ActionWake, ActionForward, ActionNotify:
	default:
		return fmt.Errorf("switchboard: listener %s has unknown action %q", l.ID, l.Action)
	}
	if l.Action == ActionWake && l.JobID == "" {
		return fmt.Errorf("switchboard: wake listener %s needs a job id", l.ID)
	}
	if l.Action == ActionForward && l.TargetQueue == "" {
		return fmt.Errorf("switchboard: forward listener %s needs a target queue", l.ID)
	}
	return nil
}

// Store keeps listeners as one JSON file each under dir.
type Store struct{ dir string }

// OpenStore creates (if needed) and opens the listener store under dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the listeners directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Create writes a new listener, refusing to overwrite one that exists.
func (s *Store) Create(l Listener) error {
	if err := l.valid(); err != nil {
		return err
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	if _, err := os.Stat(s.path(l.ID)); err == nil {
		return fmt.Errorf("switchboard: listener %s already exists", l.ID)
	}
	return writeJSONFile(s.path(l.ID), l)
}

// Remove deletes a listener. Removing one that is not there is not an error
// (a listener the job already cleaned up).
func (s *Store) Remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Get loads one listener. It returns os.ErrNotExist when there is none.
func (s *Store) Get(id string) (Listener, error) {
	var l Listener
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(b, &l)
	return l, err
}

// List returns every stored listener, sorted by queue then id so the poll
// order is stable across ticks. A corrupt or half-written file is skipped
// (writes are atomic, so this should not happen) rather than failing the
// whole poll.
func (s *Store) List() ([]Listener, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Listener
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		l, err := s.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Queue != out[j].Queue {
			return out[i].Queue < out[j].Queue
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Queues groups the stored listeners by queue: queue -> the listeners on it.
// This is what the poll loop iterates: every queue with at least one
// listener is peeked each tick.
func (s *Store) Queues() (map[string][]Listener, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	m := map[string][]Listener{}
	for _, l := range all {
		m[l.Queue] = append(m[l.Queue], l)
	}
	return m, nil
}

// writeJSONFile atomically writes v as JSON to path (tmp + rename), the same
// durability contract as the agentjob store's writes.
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
