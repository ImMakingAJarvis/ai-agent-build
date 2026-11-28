// approval_test.go covers the permission gate of startOp (card ELR-107,
// M4.3b): Allow runs the op as today, Deny finishes the op at once with
// "denied: <the matching rule>" as its tool result, and Ask parks the op
// as awaiting_approval with a pending approval while the job waits on
// the approval holding no slot.
package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

// approvalNow is the fixed clock every approval test runs on, so
// remind_at and expires_at are exact.
var approvalNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// newApprovalEnv is newTickEnv with the permission rules installed and a
// fixed clock.
func newApprovalEnv(t *testing.T, eng engine.Engine, rules perm.Rules, tools ...ops.Tool) (*Loop, *agentjob.Store) {
	t.Helper()
	l, store, _ := newTickEnv(t, eng, tools...)
	l.deps.Rules = rules
	l.deps.Now = func() time.Time { return approvalNow }
	return l, store
}

// toolReply is an assistant message carrying one tool call.
func toolReply(id, name, args string) engine.Message {
	return engine.Message{
		Role:      "assistant",
		ToolCalls: []engine.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}},
	}
}

// TestAllowRunsOp: a call the gate allows runs exactly as today — the
// runner holds the call, the op is running, the job waits on it.
func TestAllowRunsOp(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}, gotCh: got})
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "shell", `{"cmd":"ls"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case c := <-got:
		if c.Name != "shell" {
			t.Errorf("runner call = %q, want shell", c.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the allowed call never reached the runner")
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %d (err %v), want 1", len(opsList), err)
	}
	if opsList[0].Status != agentjob.OpRunning {
		t.Errorf("op status = %q, want %s", opsList[0].Status, agentjob.OpRunning)
	}
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 0 {
		t.Errorf("approvals = %d (err %v), want none", len(aps), err)
	}
}

// TestDenyReturnsDeniedResult: a deny rule matching the call finishes the
// op at once — the tool result is "denied: <the matching rule>", the
// runner never saw the call, and the job is ready for the next tick that
// shows the denial to the model.
func TestDenyReturnsDeniedResult(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{
		Deny: []perm.Rule{{Tool: "shell", Pattern: "rm*"}},
	}, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}, gotCh: got})
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "shell", `{"cmd":"rm -rf stuff"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case <-got:
		t.Fatal("the denied call reached the runner")
	default:
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %d (err %v), want 1", len(opsList), err)
	}
	op := opsList[0]
	if op.Status != agentjob.OpFailed {
		t.Fatalf("op status = %q, want %s", op.Status, agentjob.OpFailed)
	}
	if !strings.Contains(string(op.Result), "denied: shell rm*") {
		t.Errorf("op result = %s, want it to carry \"denied: shell rm*\"", op.Result)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("job status = %q, want %s (the model sees the denial next tick)", j.Status, agentjob.Ready)
	}
}

// TestAskParksOpAwaitingApproval: a call the gate cannot allow or deny is
// parked — the op is awaiting_approval, a pending approval carries the
// call, its remind and expiry times, and the job waits on the approval.
func TestAskParksOpAwaitingApproval(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "read_file", `{"path":"/etc/passwd"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %d (err %v), want 1", len(opsList), err)
	}
	if opsList[0].Status != agentjob.OpAwaitingApproval {
		t.Fatalf("op status = %q, want %s", opsList[0].Status, agentjob.OpAwaitingApproval)
	}
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %d (err %v), want 1", len(aps), err)
	}
	a := aps[0]
	if a.Status != agentjob.ApprovalPending || a.OpID != opsList[0].ID {
		t.Errorf("approval = %+v, want pending for op %s", a, opsList[0].ID)
	}
	if a.RemindAt == nil || !a.RemindAt.Equal(approvalNow.Add(time.Minute*60)) {
		t.Errorf("remind_at = %v, want %v", a.RemindAt, approvalNow.Add(time.Minute*60))
	}
	if a.ExpiresAt == nil || !a.ExpiresAt.Equal(approvalNow.Add(time.Hour*24)) {
		t.Errorf("expires_at = %v, want %v", a.ExpiresAt, approvalNow.Add(time.Hour*24))
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Waiting || j.WaitReason != agentjob.WaitApproval {
		t.Errorf("job is %q (wait_reason %q), want waiting (%s)",
			j.Status, j.WaitReason, agentjob.WaitApproval)
	}
}

// TestApprovalCarriesHashOfCall: the approval's hash is the hash of the
// exact call it covers — the same call hashes equal, a different call
// hashes different, so a decision cannot apply to another call.
func TestApprovalCarriesHashOfCall(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	args := json.RawMessage(`{"path":"/etc/passwd"}`)
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "read_file", string(args)),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %d (err %v), want 1", len(aps), err)
	}
	want := agentjob.ApprovalHash("read_file", args)
	if aps[0].Hash != want {
		t.Errorf("approval hash = %s, want %s", aps[0].Hash, want)
	}
	if other := agentjob.ApprovalHash("read_file", json.RawMessage(`{"path":"/etc/hosts"}`)); aps[0].Hash == other {
		t.Error("approval hash collides with a different call")
	}
	// The request is the call itself.
	var req struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(aps[0].Request, &req); err != nil || req.Tool != "read_file" {
		t.Errorf("approval request = %s (err %v), want the call", aps[0].Request, err)
	}
}

// TestParkedJobHoldsNoSlot: the parked job waits without a slot — the
// slot manager hands it out to any other job.
func TestParkedJobHoldsNoSlot(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "read_file", `{"path":"/etc/passwd"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil || j.Status != agentjob.Waiting {
		t.Fatalf("job = %+v (err %v), want waiting on the approval", j, err)
	}
	if _, err := l.deps.Slots.Acquire(context.Background(), "other"); err != nil {
		t.Errorf("parked job still holds the slot: Acquire(other) = %v", err)
	}
}

// TestTwentyJobsParkedHoldZeroSlots: the general consequence — twenty
// jobs each parked on their own approval leave every slot free, so the
// parking does not shrink the pool.
func TestTwentyJobsParkedHoldZeroSlots(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	for i := 1; i <= 20; i++ {
		id := "p" + string(rune('a'+i-1)) + string(rune('a'+i-1))
		if _, err := store.Ingest(agentjob.Spec{
			ID: id, Title: "park job", Instructions: "park job " + id,
			Driver: "loop", Workspace: "/ws/" + id,
		}); err != nil {
			t.Fatal(err)
		}
		eng.Script("park job "+id, fake.Step{
			Reply:        toolReply("call_1", "read_file", `{"path":"/etc/passwd"}`),
			PromptTokens: 100, OutputTokens: 10,
		})
		if err := l.tick(context.Background(), id); err != nil {
			t.Fatalf("tick %s: %v", id, err)
		}
	}
	for i := 1; i <= 20; i++ {
		id := "p" + string(rune('a'+i-1)) + string(rune('a'+i-1))
		j, err := store.Get(id)
		if err != nil || j.Status != agentjob.Waiting || j.WaitReason != agentjob.WaitApproval {
			t.Errorf("job %s = %+v (err %v), want waiting (approval)", id, j, err)
		}
		opsList, err := store.Ops(id)
		if err != nil || len(opsList) != 1 || opsList[0].Status != agentjob.OpAwaitingApproval {
			t.Errorf("job %s ops = %+v (err %v), want one awaiting approval", id, opsList, err)
		}
	}
	if _, err := l.deps.Slots.Acquire(context.Background(), "other"); err != nil {
		t.Errorf("twenty parked jobs still hold a slot: Acquire(other) = %v", err)
	}
}

// parkJob ticks the job until its one call is parked for approval, as in
// TestAskParksOpAwaitingApproval, and returns the parked op.
func parkJob(t *testing.T, l *Loop, store *agentjob.Store, eng *fake.Engine) agentjob.LoopOp {
	t.Helper()
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "read_file", `{"path":"/etc/passwd"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %d (err %v), want 1", len(opsList), err)
	}
	return opsList[0]
}

// decideAndAct records the owner's decision through the store (the
// hash-checked one-shot decide, card ELR-106) and hands the approval to
// the loop — the order the decision endpoint (card ELR-109) uses.
func decideAndAct(t *testing.T, store *agentjob.Store, l *Loop, approve bool, note string) {
	t.Helper()
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %d (err %v), want 1", len(aps), err)
	}
	if ok, err := store.DecideApproval(tickJobID, aps[0].ID, aps[0].Hash, "owner", note, approve); err != nil || !ok {
		t.Fatalf("DecideApproval: ok=%v err=%v", ok, err)
	}
	if err := l.HandleApprovalDecided(tickJobID, aps[0].ID); err != nil {
		t.Fatalf("HandleApprovalDecided: %v", err)
	}
}

// TestDecidedApprovedStartsOp: an approved decision starts the parked op
// through the run path that skips the gate — the runner holds the call,
// the op is running, and the job waits on the op, not the approval.
func TestDecidedApprovedStartsOp(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file", gotCh: got})
	parkJob(t, l, store, eng)
	decideAndAct(t, store, l, true, "ok to read")
	op, err := store.GetOp(tickJobID, "call_1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != agentjob.OpRunning {
		t.Fatalf("op status = %q, want %s", op.Status, agentjob.OpRunning)
	}
	select {
	case c := <-got:
		if c.Name != "read_file" || c.OpID != "call_1" {
			t.Errorf("runner call = %+v, want the approved op", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the approved call never reached the runner")
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Waiting || j.WaitReason != agentjob.WaitOperation {
		t.Errorf("job = %q (wait_reason %q), want waiting on the op",
			j.Status, j.WaitReason)
	}
}

// TestDecidedDeniedResultIsDeniedNote: a denied decision finishes the op
// as a failed op whose tool result is "denied: <the note>" — the model
// reads the denial as the call's result — the runner never saw the call,
// and the job is ready for the tick that shows it to the model.
func TestDecidedDeniedResultIsDeniedNote(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file", gotCh: got})
	parkJob(t, l, store, eng)
	decideAndAct(t, store, l, false, "not now")
	select {
	case <-got:
		t.Fatal("the denied call reached the runner")
	default:
	}
	op, err := store.GetOp(tickJobID, "call_1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != agentjob.OpFailed {
		t.Fatalf("op status = %q, want %s", op.Status, agentjob.OpFailed)
	}
	if !strings.Contains(string(op.Result), "denied: not now") {
		t.Errorf("op result = %s, want it to carry \"denied: not now\"", op.Result)
	}
	// The model reads the denial as the call's tool result.
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		var tc toolContent
		if json.Unmarshal(m.Content, &tc) == nil && tc.ToolCallID == "call_1" &&
			strings.Contains(tc.Content, "denied: not now") {
			found = true
		}
	}
	if !found {
		t.Errorf("no tool message carrying the denial for call_1")
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("job status = %q, want %s (the model sees the denial next tick)",
			j.Status, agentjob.Ready)
	}
}

// TestDecidedTwiceStartsOnce: the decision acts once — a repeated
// HandleApprovalDecided is a no-op and the op never starts twice.
func TestDecidedTwiceStartsOnce(t *testing.T) {
	got := make(chan ops.Call, 2)
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file", gotCh: got})
	parkJob(t, l, store, eng)
	decideAndAct(t, store, l, true, "ok")
	if err := l.HandleApprovalDecided(tickJobID, "call_1"); err != nil {
		t.Fatalf("second HandleApprovalDecided: %v (want a no-op nil)", err)
	}
	select {
	case <-got: // the first start
	case <-time.After(2 * time.Second):
		t.Fatal("the approved call never reached the runner")
	}
	select {
	case c := <-got:
		t.Fatalf("the op started twice: second call %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDecisionForCancelledJobChangesNothing: the owner decides, the job
// is cancelled before the loop acts, and the decision that then lands
// returns nil and changes nothing — the job stays parked, the op stays
// closed, and the recorded decision stays recorded.
func TestDecisionForCancelledJobChangesNothing(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	parkJob(t, l, store, eng)
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %d (err %v), want 1", len(aps), err)
	}
	if ok, err := store.DecideApproval(tickJobID, aps[0].ID, aps[0].Hash, "owner", "ok", true); err != nil || !ok {
		t.Fatalf("DecideApproval: ok=%v err=%v", ok, err)
	}
	if err := store.Stop(tickJobID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := l.HandleApprovalDecided(tickJobID, aps[0].ID); err != nil {
		t.Fatalf("HandleApprovalDecided after cancel: %v (want nil)", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Parked {
		t.Errorf("job status = %q, want %s", j.Status, agentjob.Parked)
	}
	op, err := store.GetOp(tickJobID, "call_1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != agentjob.OpCancelled {
		t.Errorf("op status = %q, want %s", op.Status, agentjob.OpCancelled)
	}
	aps, err = store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalApproved {
		t.Errorf("approval after cancel = %+v (err %v), want still approved", aps, err)
	}
}

// TestCancelClosesParkedOps: cancelling a job parked on an approval
// (Store stop) closes its parked ops as cancelled — nothing waits on a
// decision that will never come — and parks the job; a second stop is a
// no-op.
func TestCancelClosesParkedOps(t *testing.T) {
	eng := fake.New(1)
	l, store := newApprovalEnv(t, eng, perm.Rules{},
		captureTool{name: "read_file"})
	parkJob(t, l, store, eng)
	if err := store.Stop(tickJobID); err != nil {
		t.Fatalf("Stop on the parked job: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Parked {
		t.Fatalf("job status = %q, want %s", j.Status, agentjob.Parked)
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %d (err %v), want 1", len(opsList), err)
	}
	if opsList[0].Status != agentjob.OpCancelled {
		t.Errorf("op status = %q, want %s (the cancel closes the parked op)",
			opsList[0].Status, agentjob.OpCancelled)
	}
	if err := store.Stop(tickJobID); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}
