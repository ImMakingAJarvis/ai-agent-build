// Package lifecycle is the pure state-transition function of the
// event-loop runtime (ELR-1, M1.4, card ELR-16): given a job's current
// status and wait reason and the loop event that just arrived, it says
// what the status becomes. It does no I/O, touches no store and no
// broker — the loop (M1.5) applies the result to the job's state. The
// function is idempotent, which is what lets the loop.events consumer
// apply a redelivered event safely: an event whose effect the state
// already carries comes back unchanged (Changed false).
package lifecycle

import (
	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// In is the input to Transition: the state the event arrives at and the
// event itself. OpenOps and OpenChildren count the job's operations and
// child tasks that are NOT finished AFTER this event is applied — an
// op.done that finishes the last operation arrives with OpenOps 0.
type In struct {
	Status         string
	WaitReason     string
	Event          string
	OpenOps        int
	OpenChildren   int
	ModelSuspended bool
}

// Out is the state the job takes. Changed is false when the event left
// the state as it found it (a finished job, an unmatched event, or a
// wait the state already carries).
type Out struct {
	Status     string
	WaitReason string
	Changed    bool
}

// Transition decides what a job's status becomes when a loop event
// arrives. The rules (card ELR-16) are evaluated in this order; the
// first match wins:
//
//  1. a finished job (done, failed, parked) never changes;
//  2. cancel.requested parks the job;
//  3. model.suspended puts a ready or waiting job to waiting on the
//     model;
//  4. model.resumed releases a job that was waiting on the model, back
//     onto its operations or children, or to ready;
//  5. an operation, an approval or a child finishing (op.done,
//     approval.decided, task.done) moves a waiting job down the same
//     ladder — but a suspended model keeps it on the model;
//  6. user.message wakes a job that was waiting on the user;
//  7. anything else is unchanged.
func Transition(in In) Out {
	out := Out{Status: in.Status, WaitReason: in.WaitReason}
	switch {
	case in.Status == agentjob.Done || in.Status == agentjob.Failed || in.Status == agentjob.Parked:
		// Rule 1: finished is finished.
		return out
	case in.Event == queue.EvCancelRequested:
		// Rule 2.
		out.Status, out.WaitReason = agentjob.Parked, ""
	case in.Event == queue.EvModelSuspended && (in.Status == agentjob.Ready || in.Status == agentjob.Waiting):
		// Rule 3.
		out.Status, out.WaitReason = agentjob.Waiting, agentjob.WaitModel
	case in.Event == queue.EvModelResumed && in.WaitReason == agentjob.WaitModel:
		// Rule 4.
		out.Status, out.WaitReason = settle(in)
	case (in.Event == queue.EvOpDone || in.Event == queue.EvApprovalDecided || in.Event == queue.EvTaskDone) &&
		in.Status == agentjob.Waiting:
		// Rule 5: a suspended model outranks the ops/children ladder.
		if in.ModelSuspended {
			out.Status, out.WaitReason = agentjob.Waiting, agentjob.WaitModel
		} else {
			out.Status, out.WaitReason = settle(in)
		}
	case in.Event == queue.EvUserMessage && in.Status == agentjob.Waiting && in.WaitReason == agentjob.WaitUser:
		// Rule 6.
		out.Status, out.WaitReason = agentjob.Ready, ""
	}
	// Rule 7 falls through: out still holds the input state.
	out.Changed = out.Status != in.Status || out.WaitReason != in.WaitReason
	return out
}

// settle is the ops/children ladder shared by rules 4 and 5: an open
// operation keeps the job waiting on the operation, then an open child,
// then ready.
func settle(in In) (status, reason string) {
	switch {
	case in.OpenOps > 0:
		return agentjob.Waiting, agentjob.WaitOperation
	case in.OpenChildren > 0:
		return agentjob.Waiting, agentjob.WaitChild
	default:
		return agentjob.Ready, ""
	}
}
