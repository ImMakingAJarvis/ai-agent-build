package agentjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrApprovalHashMismatch: the decision names a different call than the
// approval was requested for — the given hash does not equal the stored
// Hash (ApprovalHash of the op's tool and args). Nothing is written.
var ErrApprovalHashMismatch = errors.New(
	"agentjob: approval hash mismatch: the decision is for a different call")

// ErrApprovalClosed: the job is done, failed or stopped — the task was
// finished or cancelled, so the approval has lapsed. Nothing is written.
var ErrApprovalClosed = errors.New(
	"agentjob: approval closed: the job is done, failed or stopped")

// DecideApproval records the one decision for an approval: approve or
// deny, with the decider and a note. It takes the job's flock (the one
// Claim uses) so a decision never races the loop's own approval writes.
//
// Checks, in order: unknown approval (no file) is an error; a job that is
// done, failed or stopped — the store's Parked status, the plan's
// "stopped" — has lapsed the approval (ErrApprovalClosed); a hash that
// does not equal the stored Hash is refused (ErrApprovalHashMismatch); an
// approval already approved or denied is a no-op — (false, nil), the file
// is left as the first decision wrote it (DecidedBy keeps the first
// decider). Otherwise it sets Status, DecidedBy, DecidedAt and Note and
// writes the file, returning (true, nil).
func (s *Store) DecideApproval(jobID, approvalID, hash, by, note string, approve bool) (bool, error) {
	if !ValidID(jobID) {
		return false, fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	if !validOpID(approvalID) {
		return false, fmt.Errorf("agentjob: bad approval id %q", approvalID)
	}
	lock, err := os.OpenFile(filepath.Join(s.JobDir(jobID), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	b, err := os.ReadFile(filepath.Join(s.JobDir(jobID), "approvals", approvalID+".json"))
	if err != nil {
		return false, err
	}
	var a LoopApproval
	if err := json.Unmarshal(b, &a); err != nil {
		return false, err
	}
	j, err := s.Get(jobID)
	if err != nil {
		return false, err
	}
	switch j.Status {
	case Done, Failed, Parked:
		return false, ErrApprovalClosed
	}
	if a.Hash != hash {
		return false, ErrApprovalHashMismatch
	}
	if a.Status == ApprovalApproved || a.Status == ApprovalDenied {
		return false, nil
	}
	status := ApprovalDenied
	if approve {
		status = ApprovalApproved
	}
	now := time.Now().UTC()
	a.Status = status
	a.DecidedBy = by
	a.DecidedAt = &now
	a.Note = note
	if err := s.PutApproval(jobID, a); err != nil {
		return false, err
	}
	return true, nil
}
