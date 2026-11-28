// approval_sweep_test.go covers the approval sweep (card ELR-110,
// M4.3e): a pending approval past its remind time gets exactly one
// reminder — the approval.reminder event and a note in the job's
// messages — and one past its expiry is denied "expired" with the op
// finishing as "denied: expired"; a decided approval past its expiry is
// ignored; the parked approvals of a store survive a loop restart
// (Recover) untouched, one of them resuming through an approval; and a
// sweep over nothing due changes nothing.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
)

// newSweepEnv is newApprovalEnv with a mutable clock: the tests advance
// *now to drive the remind and expiry times.
func newSweepEnv(t *testing.T, eng engine.Engine, tools ...ops.Tool) (*Loop, *agentjob.Store, *time.Time) {
	t.Helper()
	l, store := newApprovalEnv(t, eng, perm.Rules{}, tools...)
	now := approvalNow
	l.deps.Now = func() time.Time { return now }
	return l, store, &now
}

// captureEvents wires Deps.Publish to record the events it emits.
func captureEvents(t *testing.T, l *Loop) *[]string {
	t.Helper()
	var events []string
	l.deps.Publish = func(jobID, typ string) { events = append(events, jobID+"/"+typ) }
	return &events
}

// parkTick runs one tick whose tool call the gate sends to the owner,
// leaving the job parked on a pending approval (RemindAt an hour out,
// ExpiresAt a day out).
func parkTick(t *testing.T, l *Loop, eng *fake.Engine) {
	t.Helper()
	eng.Script(tickInstructions, fake.Step{
		Reply:        toolReply("call_1", "read_file", `{"path":"/etc/passwd"}`),
		PromptTokens: 100, OutputTokens: 10,
	})
	if err := l.tick(context.Background(), tickJobID); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

// TestReminderFiresOnceAfterOneHour: before the remind time the sweep
// leaves the approval alone; at and past it the sweep publishes one
// approval.reminder event and appends one note to the job's messages,
// and clears RemindAt so a later sweep fires nothing.
func TestReminderFiresOnceAfterOneHour(t *testing.T) {
	eng := fake.New(1)
	l, store, now := newSweepEnv(t, eng, captureTool{name: "read_file"})
	events := captureEvents(t, l)
	parkTick(t, l, eng)
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	base := len(msgs)

	// Before the remind time: nothing happens.
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 0 {
		t.Fatalf("events before the remind time = %v, want none", *events)
	}
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 || aps[0].RemindAt == nil {
		t.Fatalf("approvals = %+v (err %v), want one pending with RemindAt set", aps, err)
	}

	// One hour later: one reminder, then RemindAt is cleared.
	*now = approvalNow.Add(time.Hour)
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || (*events)[0] != tickJobID+"/approval.reminder" {
		t.Fatalf("events = %v, want one approval.reminder", *events)
	}
	aps, err = store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %+v (err %v), want 1", aps, err)
	}
	if aps[0].RemindAt != nil {
		t.Error("RemindAt not cleared after the reminder")
	}
	if aps[0].Status != agentjob.ApprovalPending {
		t.Errorf("status = %q, want %s (a reminder is not a decision)", aps[0].Status, agentjob.ApprovalPending)
	}
	msgs, err = store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != base+1 {
		t.Fatalf("messages = %d, want %d (one note)", len(msgs), base+1)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("note message role = %q, want user", last.Role)
	}
	var note string
	if err := json.Unmarshal(last.Content, &note); err != nil {
		t.Fatalf("note content is not a JSON string: %v", err)
	}
	if !strings.Contains(note, "approval") {
		t.Errorf("note = %q, want it to name the pending approval", note)
	}

	// And later still: it fires once.
	*now = approvalNow.Add(2 * time.Hour)
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("events = %d, want still 1 (the reminder fired once)", len(*events))
	}
	msgs, _ = store.Messages(tickJobID)
	if len(msgs) != base+1 {
		t.Fatalf("messages = %d, want %d (no second note)", len(msgs), base+1)
	}
}

// TestExpiryDeniesWithExpiredNote: a pending approval past its expiry
// is decided denied with DecidedBy "expired", the op it covers finishes
// failed with "denied: expired" as its tool result, and the job goes
// back to ready — the same shape as the owner's own denial.
func TestExpiryDeniesWithExpiredNote(t *testing.T) {
	eng := fake.New(1)
	l, store, now := newSweepEnv(t, eng, captureTool{name: "read_file"})
	events := captureEvents(t, l)
	parkTick(t, l, eng)

	*now = approvalNow.Add(25 * time.Hour)
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 0 {
		t.Fatalf("events = %v, want none (the expiry publishes no reminder)", *events)
	}
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %+v (err %v), want 1", aps, err)
	}
	if aps[0].Status != agentjob.ApprovalDenied {
		t.Errorf("status = %q, want %s", aps[0].Status, agentjob.ApprovalDenied)
	}
	if aps[0].DecidedBy != "expired" {
		t.Errorf("decided_by = %q, want \"expired\"", aps[0].DecidedBy)
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %+v (err %v), want 1", opsList, err)
	}
	if opsList[0].Status != agentjob.OpFailed {
		t.Errorf("op status = %q, want %s", opsList[0].Status, agentjob.OpFailed)
	}
	if !strings.Contains(string(opsList[0].Result), "denied: expired") {
		t.Errorf("op result = %s, want it to carry \"denied: expired\"", opsList[0].Result)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages = %d (err %v), want at least the tool message", len(msgs), err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "tool" {
		t.Fatalf("last message role = %q, want tool", last.Role)
	}
	var tc toolContent
	if err := json.Unmarshal(last.Content, &tc); err != nil {
		t.Fatalf("tool content: %v", err)
	}
	if !strings.Contains(tc.Content, "denied: expired") {
		t.Errorf("tool message = %q, want it to carry \"denied: expired\"", tc.Content)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("job status = %q, want %s (the model sees the expiry next tick)", j.Status, agentjob.Ready)
	}

	// A second sweep changes nothing: the decision is already recorded.
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	aps, _ = store.Approvals(tickJobID)
	if aps[0].DecidedBy != "expired" || aps[0].Status != agentjob.ApprovalDenied {
		t.Errorf("second sweep changed the decision: %+v", aps[0])
	}
}

// TestExpiryOfDecidedApprovalIgnored: an approval the owner already
// decided, past its expiry, is left alone by the sweep — the recorded
// decision and the op's result are not touched.
func TestExpiryOfDecidedApprovalIgnored(t *testing.T) {
	eng := fake.New(1)
	l, store, now := newSweepEnv(t, eng, captureTool{name: "read_file"})
	parkTick(t, l, eng)
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %+v (err %v), want 1", aps, err)
	}
	// The owner denies it; the loop acts on the decision.
	changed, err := store.DecideApproval(tickJobID, aps[0].ID, aps[0].Hash, "owner", "no", false)
	if err != nil || !changed {
		t.Fatalf("DecideApproval: changed=%v err=%v", changed, err)
	}
	if err := l.HandleApprovalDecided(tickJobID, aps[0].ID); err != nil {
		t.Fatal(err)
	}
	opsList, err := store.Ops(tickJobID)
	if err != nil || len(opsList) != 1 {
		t.Fatalf("ops = %+v (err %v), want 1", opsList, err)
	}
	if !strings.Contains(string(opsList[0].Result), "denied: no") {
		t.Fatalf("the owner's denial did not land: %s", opsList[0].Result)
	}

	// Past the expiry: the sweep ignores the decided approval.
	*now = approvalNow.Add(25 * time.Hour)
	if err := l.SweepApprovals(); err != nil {
		t.Fatal(err)
	}
	aps, _ = store.Approvals(tickJobID)
	if aps[0].DecidedBy != "owner" || aps[0].Note != "no" {
		t.Errorf("sweep overwrote the owner's decision: %+v", aps[0])
	}
	opsList, _ = store.Ops(tickJobID)
	if strings.Contains(string(opsList[0].Result), "expired") {
		t.Errorf("sweep rewrote the op result: %s", opsList[0].Result)
	}
}

// TestParkedApprovalsSurviveRestart: twenty jobs parked on approvals
// across a loopd restart — a fresh Loop over the same store runs
// Recover and touches none of the parked approvals (no slot is held),
// and one of them approved afterwards resumes through the normal
// decision path.
func TestParkedApprovalsSurviveRestart(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	args := json.RawMessage(`{"path":"/etc/passwd"}`)
	ids := make([]string, 0, n)
	now := time.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("park-%d", i)
		if _, err := store.Ingest(agentjob.Spec{
			ID: id, Title: "parked job", Instructions: "do it",
			Driver: "loop", Workspace: "/ws/" + id,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.PutOp(id, agentjob.LoopOp{
			ID: "op1", ToolCallID: "call_1", Tool: "read_file",
			Args: args, Status: agentjob.OpAwaitingApproval,
		}); err != nil {
			t.Fatal(err)
		}
		remind, expire := now.Add(time.Hour), now.Add(24*time.Hour)
		if err := store.PutApproval(id, agentjob.LoopApproval{
			ID: "op1", OpID: "op1",
			Request:  json.RawMessage(`{"tool":"read_file","args":{"path":"/etc/passwd"}}`),
			Status:   agentjob.ApprovalPending,
			Hash:     agentjob.ApprovalHash("read_file", args),
			RemindAt: &remind, ExpiresAt: &expire,
		}); err != nil {
			t.Fatal(err)
		}
		j, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		st := j.State
		st.Status = agentjob.Waiting
		st.WaitReason = agentjob.WaitApproval
		if err := store.SetStatus(id, st); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// The restarted daemon: a fresh Loop over the same store.
	eng := fake.New(n)
	reg := ops.NewRegistry(captureTool{name: "read_file",
		res: ops.Result{Status: agentjob.OpDone, Output: "ok"}})
	runner := &ops.Runner{Registry: reg}
	gj := &gateJobs{states: map[string]slot.JobState{}, warm: map[string]int{}}
	cfg := config.Default()
	l := New(Deps{
		Engine: eng, Slots: slot.New(slot.Config{Slots: cfg.Slots,
			SnapshotMinTokens: cfg.SnapshotMinTokens}, eng, gj),
		Runner: runner, Registry: reg, Store: store, Config: cfg, Now: time.Now,
	})
	runner.OnDone = func(jobID, opID string, res ops.Result) { _ = l.HandleOpDone(jobID, opID, res) }

	if err := l.Recover(ids); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// No slot is held: a probe job can still acquire the (single) slot.
	gj.states["probe"] = slot.JobState{ID: "probe"}
	lease, err := l.deps.Slots.Acquire(context.Background(), "probe")
	if err != nil {
		t.Fatalf("Recover left a slot held: probe acquire: %v", err)
	}
	l.deps.Slots.Release(lease, 0)

	// Every parked job still waits on its pending approval.
	for _, id := range ids {
		j, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != agentjob.Waiting || j.WaitReason != agentjob.WaitApproval {
			t.Errorf("%s: status = %q/%q, want %s/%s",
				id, j.Status, j.WaitReason, agentjob.Waiting, agentjob.WaitApproval)
		}
		aps, err := store.Approvals(id)
		if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalPending {
			t.Errorf("%s: approvals = %+v (err %v), want one pending", id, aps, err)
		}
		opsList, err := store.Ops(id)
		if err != nil || len(opsList) != 1 || opsList[0].Status != agentjob.OpAwaitingApproval {
			t.Errorf("%s: ops = %+v (err %v), want one awaiting_approval", id, opsList, err)
		}
	}

	// One approval through the decision path resumes its job.
	changed, err := store.DecideApproval(ids[0], "op1",
		agentjob.ApprovalHash("read_file", args), "owner", "ok", true)
	if err != nil || !changed {
		t.Fatalf("DecideApproval: changed=%v err=%v", changed, err)
	}
	if err := l.HandleApprovalDecided(ids[0], "op1"); err != nil {
		t.Fatalf("HandleApprovalDecided: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := store.Get(ids[0])
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == agentjob.Ready {
			break // the op ran and the job is back in the queue
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not resume: status %q", ids[0], j.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The other jobs are still parked, untouched.
	for _, id := range ids[1:] {
		j, _ := store.Get(id)
		if j.Status != agentjob.Waiting {
			t.Errorf("%s: status = %q, want %s", id, j.Status, agentjob.Waiting)
		}
	}
}

// TestSweepIsIdempotent: a sweep over a store with nothing due — no
// reminder due, no expiry due — changes nothing, and sweeping again
// changes nothing.
func TestSweepIsIdempotent(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newSweepEnv(t, eng, captureTool{name: "read_file"})
	events := captureEvents(t, l)
	parkTick(t, l, eng)
	aps, err := store.Approvals(tickJobID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %+v (err %v), want 1", aps, err)
	}
	first := aps[0]
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(msgs)

	for pass := 0; pass < 2; pass++ {
		if err := l.SweepApprovals(); err != nil {
			t.Fatalf("sweep pass %d: %v", pass, err)
		}
	}
	if len(*events) != 0 {
		t.Fatalf("events = %v, want none (nothing was due)", *events)
	}
	aps, _ = store.Approvals(tickJobID)
	if len(aps) != 1 || aps[0].Status != first.Status ||
		aps[0].DecidedBy != first.DecidedBy ||
		aps[0].RemindAt == nil || first.RemindAt == nil ||
		!aps[0].RemindAt.Equal(*first.RemindAt) {
		t.Errorf("sweep changed the approval: before %+v after %+v", first, aps)
	}
	msgs, _ = store.Messages(tickJobID)
	if len(msgs) != before {
		t.Errorf("messages = %d, want %d (no notes)", len(msgs), before)
	}
}
