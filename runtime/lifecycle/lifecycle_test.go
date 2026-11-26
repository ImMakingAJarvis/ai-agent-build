package lifecycle

import (
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func TestTransition(t *testing.T) {
	tests := []struct {
		name string
		in   In
		want Out
	}{
		// Rule 1: a finished job never changes.
		{name: "done ignores op.done",
			in:   In{Status: agentjob.Done, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Done}},
		{name: "failed ignores cancel.requested",
			in:   In{Status: agentjob.Failed, Event: queue.EvCancelRequested},
			want: Out{Status: agentjob.Failed}},
		{name: "parked ignores model.suspended",
			in:   In{Status: agentjob.Parked, Event: queue.EvModelSuspended},
			want: Out{Status: agentjob.Parked}},

		// Rule 2: cancel.requested parks the job.
		{name: "cancel parks ready",
			in:   In{Status: agentjob.Ready, Event: queue.EvCancelRequested},
			want: Out{Status: agentjob.Parked, Changed: true}},
		{name: "cancel parks running",
			in:   In{Status: agentjob.Running, Event: queue.EvCancelRequested},
			want: Out{Status: agentjob.Parked, Changed: true}},
		{name: "cancel parks a job waiting on an op",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenOps: 1, Event: queue.EvCancelRequested},
			want: Out{Status: agentjob.Parked, Changed: true}},

		// Rule 3: model.suspended puts ready or waiting jobs on the model.
		{name: "suspend puts ready on the model",
			in:   In{Status: agentjob.Ready, Event: queue.EvModelSuspended},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, Changed: true}},
		{name: "suspend takes a job off its op",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenOps: 1, Event: queue.EvModelSuspended},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, Changed: true}},

		// Rule 4: model.resumed releases a job waiting on the model.
		{name: "resume with nothing open goes ready",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, Event: queue.EvModelResumed},
			want: Out{Status: agentjob.Ready, Changed: true}},
		{name: "resume with an open op goes waiting/operation",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, OpenOps: 2, Event: queue.EvModelResumed},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, Changed: true}},
		{name: "resume with only open children goes waiting/child",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, OpenChildren: 3, Event: queue.EvModelResumed},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitChild, Changed: true}},
		{name: "resume is a no-op when not waiting on the model",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenOps: 1, Event: queue.EvModelResumed},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation}},

		// Rule 5: an op, an approval or a child finishing moves a waiting
		// job down the ladder.
		{name: "last op done wakes the job",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Ready, Changed: true}},
		{name: "op done with one of two ops still open stays waiting",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenOps: 1, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation}},
		{name: "ops drained but a child is still open",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenChildren: 2, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitChild, Changed: true}},
		{name: "last child done wakes the job",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitChild, Event: queue.EvTaskDone},
			want: Out{Status: agentjob.Ready, Changed: true}},
		{name: "child done with a sibling still open stays waiting",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitChild, OpenChildren: 1, Event: queue.EvTaskDone},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitChild}},
		{name: "approval decided wakes the job",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitApproval, Event: queue.EvApprovalDecided},
			want: Out{Status: agentjob.Ready, Changed: true}},
		{name: "suspended model outranks a finished op",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, ModelSuspended: true, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitModel, Changed: true}},
		{name: "op done on a running job is a no-op",
			in:   In{Status: agentjob.Running, Event: queue.EvOpDone},
			want: Out{Status: agentjob.Running}},

		// Rule 6: user.message wakes a job waiting on the user.
		{name: "user message wakes a user wait",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitUser, Event: queue.EvUserMessage},
			want: Out{Status: agentjob.Ready, Changed: true}},
		{name: "user message on an op wait is a no-op",
			in:   In{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation, OpenOps: 1, Event: queue.EvUserMessage},
			want: Out{Status: agentjob.Waiting, WaitReason: agentjob.WaitOperation}},
		{name: "user message on ready is a no-op",
			in:   In{Status: agentjob.Ready, Event: queue.EvUserMessage},
			want: Out{Status: agentjob.Ready}},

		// Rule 7: anything else is unchanged.
		{name: "unknown event leaves ready alone",
			in:   In{Status: agentjob.Ready, Event: queue.EvTimerFired},
			want: Out{Status: agentjob.Ready}},
		{name: "no event leaves running alone",
			in:   In{Status: agentjob.Running},
			want: Out{Status: agentjob.Running}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Transition(tt.in); got != tt.want {
				t.Errorf("Transition(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
