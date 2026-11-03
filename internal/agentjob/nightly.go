package agentjob

import "fmt"

// NoBrakes support: the parked state and the nightly tag.
//
// A nightly-tagged job (TagNightly) is stored parked — present in the
// store but out of the active queue (Next and the dashboard queue view
// never see it). The NoBrakes nightly gate (internal/nobrakes.AutoEnqueue,
// driven by the agent-run loop each iteration) reads the Redis auto_mode
// flag and, when it is on and today's UTC nightly window is unprocessed,
// enqueues every parked nightly job (moves it to ready). The gate is the
// only thing that moves a parked job; manually, `jarvis-jobs requeue <id>`
// does the same.

// ParkedNightly returns every parked job tagged nightly (TagNightly), in
// queue order. These are the jobs waiting for the NoBrakes nightly gate.
func (s *Store) ParkedNightly() ([]Job, error) {
	jobs, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range jobs {
		if j.Status == Parked && j.HasTag(TagNightly) {
			out = append(out, j)
		}
	}
	return out, nil
}

// Enqueue moves a parked job to ready — into the active queue. It is the
// manual counterpart of the nightly gate: a parked job the owner wants
// now, without waiting for the window. It refuses jobs that are not
// parked, so it can never race an in-flight runner.
func (s *Store) Enqueue(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("agentjob: bad job id %q", id)
	}
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Parked {
		return fmt.Errorf("agentjob: job %s is %s, not parked", id, j.Status)
	}
	j.Status = Ready
	return s.setState(id, j.State)
}
