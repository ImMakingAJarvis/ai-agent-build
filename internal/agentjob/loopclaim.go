// ClaimLoop and the dead-claimer rule (card ELR-111, M4.4a).
package agentjob

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DriverLoop is the RanWith of a job claimed by the event-loop runtime
// (loopd). loopd runs in a container whose PID numbers mean nothing on
// the host, so a loop claim records PID 0 on purpose and is judged dead
// by the runtime lock, never by a PID.
const DriverLoop = "loop"

// ClaimLoop marks a ready job as claimed by the loop runtime. It takes
// the same flock on the job's lock file as Claim, so two ClaimLoops — or
// a Claim and a ClaimLoop — can never take the same job: the loser reads
// the winner's Claimed state under the lock and is refused. The recorded
// PID is 0 on purpose: loopd runs in a container whose PID numbers mean
// nothing on the host, so the claim is judged dead by the runtime lock
// (claimDead), never by a PID.
func (s *Store) ClaimLoop(id, round string) (Job, error) {
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
	if j.Status != Ready {
		return j, fmt.Errorf("job %s is %s, not ready", id, j.Status)
	}
	j.State = State{Status: Claimed, RanWith: DriverLoop, Round: round, PID: 0,
		ClaimedAt: time.Now().UTC().Format(time.RFC3339)}
	return j, s.setState(id, j.State)
}

// claimDead is the dead-claimer rule (M4.4a): has the owner of a
// claimed or running job gone, so its claim may be taken over? A loop
// claim (RanWith == DriverLoop) is dead when nobody holds
// <store>/loop.lock — the same file runtime/lock.Acquire takes and
// loopd holds for its whole life; the kernel releases the flock when
// loopd dies. alive(PID) is not consulted for these jobs: the loop's
// PID is 0 on purpose. Every other job keeps the PID rule unchanged.
func (s *Store) claimDead(j Job) bool {
	if j.RanWith == DriverLoop {
		return !s.loopLockHeld()
	}
	return !alive(j.PID)
}

// loopLockHeld probes <store>/loop.lock with a non-blocking exclusive
// flock: true while someone (loopd) holds the runtime lock. A probe
// that cannot open the file counts as held: never take over a claim on
// an error.
func (s *Store) loopLockHeld() bool {
	f, err := os.OpenFile(filepath.Join(s.dir, "loop.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK: someone holds it. Any other error: assume held —
		// never take over a claim on a probe failure.
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
