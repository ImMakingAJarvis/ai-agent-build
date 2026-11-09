package agentjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Result is one arrived result for a WaitOn correlation id. The switchboard
// (plan task 14) writes these to the job's results sidecar (results.json)
// via Store.RecordResult when a message lands on the job's reply queue.
type Result struct {
	Corr string    `json:"corr"`
	Ref  string    `json:"ref"` // where the result is: file path, message id, ...
	At   time.Time `json:"at"`
}

// WakeRecord is the wake record (wake.json) written when a waiting job is
// woken back to ready. Reason says WHY it woke: WakeAll (every WaitOn
// result arrived) or WakeTimeout (the WaitUntil deadline passed first — the
// woken round must read this record before trusting any results).
type WakeRecord struct {
	At      string            `json:"at"`
	Reason  string            `json:"reason"` // WakeAll or WakeTimeout
	Results map[string]string `json:"results,omitempty"` // corr -> ref (reason WakeAll)
	Trigger string            `json:"trigger,omitempty"` // resultRef passed to Wake
}

// Wake reasons.
const (
	WakeAll     = "all"
	WakeTimeout = "timeout"
)

// ErrWaitOnUnmet is returned by Wake when some WaitOn results have not
// arrived yet and the WaitUntil deadline has not passed: the job stays
// waiting.
var ErrWaitOnUnmet = errors.New("agentjob: wait_on results not all in, deadline not reached")

// Yield transitions a running job to waiting: a brain round ending ON
// PURPOSE — the job is not done and not failed, it is parked on the results
// named by waitOn (correlation ids on its reply queue). waitUntil is the
// deadline: every waiting job has one (invariant 2), so Yield refuses a
// zero or past deadline. note is the round's note for its successor.
// It refuses jobs that are not running: only a round in flight can yield.
//
// waitUntil is part of the call (rather than a separate setter) so the
// invariant cannot be violated by forgetting a step.
func (s *Store) Yield(id string, waitOn []string, waitUntil time.Time, note string) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	if waitUntil.IsZero() || !waitUntil.After(time.Now()) {
		return fmt.Errorf("agentjob: job %s needs a future wait deadline (every waiting job has one)", id)
	}
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Running {
		return fmt.Errorf("agentjob: job %s is %s, not running", id, j.Status)
	}
	j.WaitOn = waitOn
	j.WaitUntil = &waitUntil
	if err := writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec); err != nil {
		return err
	}
	j.Status = Waiting
	j.WaitingSince = time.Now().UTC().Format(time.RFC3339)
	j.WaitNote = note
	return s.setState(id, j.State)
}

// RecordResult records that the result for correlation id corr has arrived;
// ref points at it (file path, message id, ...). The switchboard writes the
// sidecar when a message lands on a waiting job's reply queue; Wake consults
// it to decide whether all WaitOn are satisfied. Recording the same corr
// again replaces its ref (a redelivered result must not block the wake).
func (s *Store) RecordResult(id, corr, ref string) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	if corr == "" {
		return fmt.Errorf("agentjob: empty correlation id for job %s", id)
	}
	res, err := s.results(id)
	if err != nil {
		return err
	}
	if res == nil {
		res = map[string]Result{}
	}
	res[corr] = Result{Corr: corr, Ref: ref, At: time.Now().UTC()}
	if err := writeJSON(filepath.Join(s.JobDir(id), "results.json"), res); err != nil {
		return err
	}
	// Keep the continuation's result refs in step (plan §3.5): the woken
	// round reads continuation.json for the corr -> ref map. Best-effort —
	// results.json is the authoritative sidecar Wake consults, so a missing
	// or corrupt continuation never blocks recording a result.
	if c, err := s.ReadContinuation(id); err == nil && c != nil {
		if c.ResultRefs == nil {
			c.ResultRefs = map[string]string{}
		}
		c.ResultRefs[corr] = ref
		_ = s.WriteContinuation(c)
	}
	return nil
}

// results loads the job's results sidecar. A missing file is not an error:
// no results have arrived yet.
func (s *Store) results(id string) (map[string]Result, error) {
	b, err := os.ReadFile(filepath.Join(s.JobDir(id), "results.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var res map[string]Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// Wake wakes a waiting job back to ready, bumping its priority so it
// resumes before new work. It wakes only when:
//   - every WaitOn correlation id has a recorded result (reason WakeAll), or
//   - the WaitUntil deadline has passed (reason WakeTimeout — the woken
//     round must read the wake record before using results, which may be
//     partial).
//
// The wake record (wake.json) says which. A wake that finds the results
// still missing and the deadline not yet reached leaves the job waiting and
// returns ErrWaitOnUnmet. A double wake — or a wake of a job that is not
// waiting — is a no-op returning the existing record (empty when there is
// none). resultRef identifies the result this wake attempt was triggered
// by ("" for a manual wake); it is recorded in the wake record's Trigger.
func (s *Store) Wake(id, resultRef string) (WakeRecord, error) {
	if !ValidID(id) {
		return WakeRecord{}, fmt.Errorf("agentjob: bad job id %q", id)
	}
	j, err := s.Get(id)
	if err != nil {
		return WakeRecord{}, err
	}
	if j.Status != Waiting {
		// Already woken (or never waited): a no-op. A stray result
		// delivery for a job that no longer waits is not an error.
		rec, err := s.ReadWake(id)
		if errors.Is(err, os.ErrNotExist) {
			return WakeRecord{}, nil
		}
		return rec, err
	}
	res, err := s.results(id)
	if err != nil {
		return WakeRecord{}, err
	}
	now := time.Now().UTC()
	reason := WakeAll
	for _, corr := range j.WaitOn {
		if _, ok := res[corr]; !ok {
			if dl := j.WaitDeadline(); !dl.IsZero() && now.After(dl) {
				reason = WakeTimeout
				break
			}
			return WakeRecord{}, ErrWaitOnUnmet
		}
	}
	// Priority bump: the resumed job must run before new work.
	j.Priority = s.bumpPriority(id, j.Priority)
	if err := writeJSON(filepath.Join(s.JobDir(id), "job.json"), j.Spec); err != nil {
		return WakeRecord{}, err
	}
	rec := WakeRecord{At: now.Format(time.RFC3339), Reason: reason, Trigger: resultRef}
	if reason == WakeAll {
		rec.Results = map[string]string{}
		for _, corr := range j.WaitOn {
			rec.Results[corr] = res[corr].Ref
		}
	}
	// The record lands before the status flips: a crash in between leaves a
	// waiting job with a wake record, and the next Wake repeats it.
	if err := writeJSON(filepath.Join(s.JobDir(id), "wake.json"), rec); err != nil {
		return WakeRecord{}, err
	}
	j.Status = Ready
	if err := s.setState(id, j.State); err != nil {
		return WakeRecord{}, err
	}
	return rec, nil
}

// ReadWake loads the wake record (wake.json) written by Wake. It returns
// os.ErrNotExist when the job has never been woken.
func (s *Store) ReadWake(id string) (WakeRecord, error) {
	var rec WakeRecord
	b, err := os.ReadFile(filepath.Join(s.JobDir(id), "wake.json"))
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(b, &rec)
	return rec, err
}

// bumpPriority returns a priority that sorts the woken job ahead of every
// job currently ready: one above the highest ready priority (and above the
// job's own, so a re-yielded job still jumps the queue it was bumped to).
func (s *Store) bumpPriority(id string, own int) int {
	jobs, err := s.List()
	if err != nil {
		return own + 1
	}
	max := own
	for _, j := range jobs {
		if j.ID != id && j.Status == Ready && j.Priority > max {
			max = j.Priority
		}
	}
	return max + 1
}
