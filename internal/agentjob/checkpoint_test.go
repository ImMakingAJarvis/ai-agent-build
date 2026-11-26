package agentjob

import (
	"encoding/json"
	"errors"
	"testing"
)

// The checkpoint tests are written before the implementation (ELR M1.2d,
// tests first): card m1-2e makes Checkpoint and RecoverLoop pass them.
// Every test below starts skipped until then.

// newRunningJob opens a fresh store and leaves job j1 in status running
// — the state a loop tick finds it in before a checkpoint.
func newRunningJob(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(Spec{ID: "j1", Title: "checkpoint job"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("j1", State{Status: Running}); err != nil {
		t.Fatal(err)
	}
	return s
}

// toolCallCheckpoint is the tick the tests replay: one assistant message
// that starts a shell tool call, the op it starts, and the waiting state
// the tick ends in.
func toolCallCheckpoint() Checkpoint {
	return Checkpoint{
		Messages: []LoopMessage{{
			Role:    "assistant",
			Content: json.RawMessage(`{"tool_calls":[{"id":"call_1","name":"shell","arguments":{"cmd":"ls"}}]}`),
		}},
		Ops:   []LoopOp{{ID: "op1", ToolCallID: "call_1", Tool: "shell", Status: OpPending}},
		State: State{Status: Waiting, WaitReason: WaitOperation},
	}
}

// TestCheckpointHappy: one tick lands all three parts — the message, the
// op it starts, and the new state — and leaves the job waiting on the op.
func TestCheckpointHappy(t *testing.T) {
	t.Cleanup(func() { failAfter = 0 })
	s := newRunningJob(t)
	if err := s.Checkpoint("j1", toolCallCheckpoint()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	msgs, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Messages has %d entries, want 1", len(msgs))
	}
	ops, err := s.Ops("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("Ops has %d, want 1", len(ops))
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Waiting || j.WaitReason != WaitOperation {
		t.Errorf("job is %s (wait_reason %q), want waiting (%s)",
			j.Status, j.WaitReason, WaitOperation)
	}
}

// TestCheckpointCrashAfterMessages: a crash after the messages land but
// before the ops — RecoverLoop rebuilds the op from the tool call and
// puts the job back to waiting on it.
func TestCheckpointCrashAfterMessages(t *testing.T) {
	t.Cleanup(func() { failAfter = 0 })
	s := newRunningJob(t)
	failAfter = 1
	if err := s.Checkpoint("j1", toolCallCheckpoint()); !errors.Is(err, errInjected) {
		t.Fatalf("Checkpoint err = %v, want errInjected", err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("RecoverLoop: %v", err)
	}
	msgs, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Messages has %d entries, want 1", len(msgs))
	}
	ops, err := s.Ops("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("Ops has %d, want 1", len(ops))
	}
	if ops[0].ID != "call_1" || ops[0].Tool != "shell" || ops[0].Status != OpPending {
		t.Errorf("rebuilt op = %s/%s/%s, want call_1/shell/%s",
			ops[0].ID, ops[0].Tool, ops[0].Status, OpPending)
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Waiting || j.WaitReason != WaitOperation {
		t.Errorf("job is %s (wait_reason %q), want waiting (%s)",
			j.Status, j.WaitReason, WaitOperation)
	}
}

// TestCheckpointCrashAfterOps: a crash after the ops land but before the
// state — RecoverLoop reaches the same end result as the happy case.
func TestCheckpointCrashAfterOps(t *testing.T) {
	t.Cleanup(func() { failAfter = 0 })
	s := newRunningJob(t)
	failAfter = 2
	if err := s.Checkpoint("j1", toolCallCheckpoint()); !errors.Is(err, errInjected) {
		t.Fatalf("Checkpoint err = %v, want errInjected", err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("RecoverLoop: %v", err)
	}
	msgs, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Messages has %d entries, want 1", len(msgs))
	}
	ops, err := s.Ops("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("Ops has %d, want 1", len(ops))
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Waiting || j.WaitReason != WaitOperation {
		t.Errorf("job is %s (wait_reason %q), want waiting (%s)",
			j.Status, j.WaitReason, WaitOperation)
	}
}

// TestRecoverLoopNoToolCalls: the last message has no tool calls, so
// nothing to rebuild — RecoverLoop puts the job back to ready and the
// tick must be decided again by the loop.
func TestRecoverLoopNoToolCalls(t *testing.T) {
	t.Cleanup(func() { failAfter = 0 })
	s := newRunningJob(t)
	if _, err := s.AppendMessages("j1", LoopMessage{
		Role:    "assistant",
		Content: json.RawMessage(`"all done"`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("RecoverLoop: %v", err)
	}
	j, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Ready {
		t.Errorf("status = %s, want %s", j.Status, Ready)
	}
}

// TestRecoverLoopIdempotent: a second RecoverLoop changes nothing — it
// must not rebuild the op the first call already wrote.
func TestRecoverLoopIdempotent(t *testing.T) {
	t.Cleanup(func() { failAfter = 0 })
	s := newRunningJob(t)
	failAfter = 1
	if err := s.Checkpoint("j1", toolCallCheckpoint()); !errors.Is(err, errInjected) {
		t.Fatalf("Checkpoint err = %v, want errInjected", err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("first RecoverLoop: %v", err)
	}
	msgs1, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	ops1, err := s.Ops("j1")
	if err != nil {
		t.Fatal(err)
	}
	j1, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverLoop("j1"); err != nil {
		t.Fatalf("second RecoverLoop: %v", err)
	}
	msgs2, err := s.Messages("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != len(msgs1) {
		t.Fatalf("Messages has %d entries after the second call, want %d",
			len(msgs2), len(msgs1))
	}
	ops2, err := s.Ops("j1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops2) != len(ops1) {
		t.Fatalf("Ops has %d after the second call, want %d (no rebuild)",
			len(ops2), len(ops1))
	}
	for i := range ops1 {
		if ops2[i].ID != ops1[i].ID || ops2[i].Status != ops1[i].Status {
			t.Errorf("ops[%d] = %s/%s after the second call, want %s/%s",
				i, ops2[i].ID, ops2[i].Status, ops1[i].ID, ops1[i].Status)
		}
	}
	j2, err := s.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if j2.Status != j1.Status || j2.WaitReason != j1.WaitReason {
		t.Errorf("job is %s (wait_reason %q) after the second call, want %s (wait_reason %q)",
			j2.Status, j2.WaitReason, j1.Status, j1.WaitReason)
	}
}
