// approval.go is the loop side of the approval round trip (card ELR-108,
// M4.3c): the owner's decision is already recorded on the approval by
// DecideApproval (card ELR-106), which checked the decision's hash
// against the exact call the approval covers. HandleApprovalDecided only
// acts on it: an approved op starts through the run path that skips the
// permission gate, a denied one finishes as a failed op whose tool result
// is "denied: <the note>" — the model reads it as that call's result —
// and a job cancelled in the meantime changes nothing.
package loop

import (
	"errors"
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// HandleApprovalDecided acts on the decision recorded for the approval
// named approvalID (card ELR-108): approved starts the op it covers —
// the owner already decided, and DecideApproval checked the hash — and a
// denied one finishes it as a failed op carrying "denied: <the note>" so
// the model sees the denial as the call's result and the job goes back to
// ready either way it can wake on.
//
// It changes nothing for an op that no longer waits on the approval: the
// job was cancelled meanwhile (the op is closed) or an earlier pass
// already started or finished it, and a repeated call for a decided
// approval is that same no-op. A job that is done, failed or parked
// lapses the decision with it (DecideApproval refuses to record one).
func (l *Loop) HandleApprovalDecided(jobID, approvalID string) error {
	aps, err := l.deps.Store.Approvals(jobID)
	if err != nil {
		return err
	}
	var a *agentjob.LoopApproval
	for i := range aps {
		if aps[i].ID == approvalID {
			a = &aps[i]
			break
		}
	}
	if a == nil {
		return fmt.Errorf("loop: approval %s of job %s not found", approvalID, jobID)
	}
	if a.Status == agentjob.ApprovalPending {
		return fmt.Errorf("loop: approval %s of job %s has no decision yet", approvalID, jobID)
	}
	op, err := l.deps.Store.GetOp(jobID, a.OpID)
	if err != nil {
		return err
	}
	if op.Status != agentjob.OpAwaitingApproval {
		// The op no longer waits on the approval: an earlier pass
		// started or finished it, or a cancel closed it. Nothing changes.
		return nil
	}
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	switch j.Status {
	case agentjob.Done, agentjob.Failed, agentjob.Parked:
		// The job left the queue; the decision lapses with it.
		return nil
	}
	switch a.Status {
	case agentjob.ApprovalApproved:
		return l.startApprovedOp(jobID, op)
	case agentjob.ApprovalDenied:
		// The tool message for a non-done result is rendered from
		// Result.Error in HandleOpDone, so the note goes in both.
		return l.HandleOpDone(jobID, op.ID, ops.Result{
			Status: agentjob.OpFailed,
			Output: "denied: " + a.Note,
			Error:  "denied: " + a.Note,
		})
	default:
		return fmt.Errorf("loop: approval %s of job %s has unknown status %q",
			approvalID, jobID, a.Status)
	}
}

// startApprovedOp starts an op the owner approved: the tail of startOp's
// run path — mark it running and hand the call to the Runner — without
// the permission gate, which the owner's decision replaces. The job then
// waits on the op, not on the approval.
func (l *Loop) startApprovedOp(jobID string, op agentjob.LoopOp) error {
	job, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	now := l.deps.Now()
	op.Status = agentjob.OpRunning
	op.StartedAt = &now
	if err := l.deps.Store.PutOp(jobID, op); err != nil {
		return err
	}
	if err := l.deps.Runner.Start(ops.Call{
		Name:      op.Tool,
		JobID:     jobID,
		OpID:      op.ID,
		Workspace: job.Spec.Workspace,
		Args:      op.Args,
		LogPath:   l.deps.Store.OpLogPath(jobID, op.ID),
		OnPID: func(pid int) {
			cur, err := l.deps.Store.GetOp(jobID, op.ID)
			if err != nil {
				return
			}
			cur.PID = pid
			_ = l.deps.Store.PutOp(jobID, cur)
		},
	}); err != nil {
		return err
	}
	l.markOpStarted(op.ID)
	st := job.State
	st.Status = agentjob.Waiting
	st.WaitReason = agentjob.WaitOperation
	return l.deps.Store.SetStatus(jobID, st)
}

// SweepApprovals tends every pending approval in the store (card
// ELR-110, M4.3e). It is called from Step before the step picks work:
// a pending approval past its RemindAt gets one reminder — an
// approval.reminder event on Deps.Publish and a note appended to the
// job's messages — and its RemindAt is cleared so it fires once; a
// pending approval past its ExpiresAt is decided denied with DecidedBy
// "expired" and the op it covers finishes with "denied: expired", the
// same path as the owner's own denial.
//
// It sees the approvals of jobs that wait on the owner while holding no
// slot — including the ones a restart left behind: Recover puts the
// jobs back on the rails, the sweep tends their approvals. A job that
// is done, failed or parked has lapsed its approvals and is skipped.
func (l *Loop) SweepApprovals() error {
	jobs, err := l.deps.Store.List()
	if err != nil {
		return err
	}
	now := l.deps.Now()
	for _, j := range jobs {
		switch j.Status {
		case agentjob.Done, agentjob.Failed, agentjob.Parked:
			continue // the approval lapsed with the job
		}
		aps, err := l.deps.Store.Approvals(j.ID)
		if err != nil {
			return err
		}
		for i := range aps {
			a := aps[i]
			if a.Status != agentjob.ApprovalPending {
				continue
			}
			// The expiry beats the reminder: a call the owner never
			// answered is denied, not nudged.
			if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
				if err := l.expireApproval(j.ID, a); err != nil {
					return err
				}
				continue
			}
			if a.RemindAt != nil && !now.Before(*a.RemindAt) {
				if err := l.remindApproval(j.ID, a); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// remindApproval fires the one reminder for a pending approval past its
// RemindAt: the approval.reminder event (Deps.Publish, when set), the
// note in the job's messages (the model sees it on its next tick), and
// the cleared RemindAt that keeps it from firing again.
func (l *Loop) remindApproval(jobID string, a agentjob.LoopApproval) error {
	if l.deps.Publish != nil {
		l.deps.Publish(jobID, "approval.reminder")
	}
	op, err := l.deps.Store.GetOp(jobID, a.OpID)
	if err != nil {
		return err
	}
	note := "[runtime] your " + op.Tool + " call is still waiting on the owner's approval."
	if a.ExpiresAt != nil {
		note += " It will be denied as expired if no decision arrives by " +
			a.ExpiresAt.Format(time.RFC3339) + "."
	}
	if err := l.appendUserNote(jobID, note); err != nil {
		return err
	}
	a.RemindAt = nil
	return l.deps.Store.PutApproval(jobID, a)
}

// expireApproval denies a pending approval past its ExpiresAt: the
// decision is recorded with DecidedBy "expired" through the same
// DecideApproval the owner's decision uses (the approval's own hash
// binds it to the exact call it covers), and HandleApprovalDecided
// finishes the op it covers with "denied: expired". A job that left the
// queue in the meantime has lapsed the approval and is skipped, not an
// error; a decision already recorded is a no-op (DecideApproval
// reports it and the op no longer waits on the approval).
func (l *Loop) expireApproval(jobID string, a agentjob.LoopApproval) error {
	_, err := l.deps.Store.DecideApproval(jobID, a.ID, a.Hash, "expired", "expired", false)
	if err != nil {
		if errors.Is(err, agentjob.ErrApprovalClosed) {
			return nil // the job is done, failed or parked
		}
		return err
	}
	return l.HandleApprovalDecided(jobID, a.ID)
}
