// recover_test.go covers card m3-1b (ELR-32): after loopd restarts, a
// running op whose process is gone is lost (the model reads "lost"), a
// running op whose process survived is adopted and the job comes back
// ready when the process exits, a pending op is started again through
// the same path the tick uses, and every job that ends up ready is
// submitted.
package loop

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// recoverSetup builds a loop over a fresh store whose one job has a last
// tick with a single tool call (callID to the named tool), that call's op
// written with the given status and PID, and the job waiting on it — the
// shape a restart finds. Extra tools are registered after the default
// one (the registry keeps the last).
func recoverSetup(t *testing.T, eng engine.Engine, callID, tool, status string, pid int, tools ...ops.Tool) (*Loop, *agentjob.Store) {
	t.Helper()
	all := append([]ops.Tool{captureTool{name: tool}}, tools...)
	l, store, _ := newTickEnv(t, eng, all...)
	content, err := json.Marshal(assistantContent{
		ToolCalls: []engine.ToolCall{{ID: callID, Name: tool, Arguments: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessages(tickJobID, agentjob.LoopMessage{Role: "assistant", Content: content}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp(tickJobID, agentjob.LoopOp{
		ID: callID, ToolCallID: callID, Tool: tool,
		Args: json.RawMessage(`{}`), Status: status, PID: pid,
	}); err != nil {
		t.Fatal(err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	j.Status = agentjob.Waiting
	j.WaitReason = agentjob.WaitOperation
	if err := store.SetStatus(tickJobID, j.State); err != nil {
		t.Fatal(err)
	}
	return l, store
}

// TestRecoverLostOp: an op running with PID 0 cannot be adopted — it is
// lost, the job gets a tool message containing "lost", and it becomes
// ready.
func TestRecoverLostOp(t *testing.T) {
	eng := fake.New(1)
	l, store := recoverSetup(t, eng, "call_1", "shell", agentjob.OpRunning, 0)
	if err := l.Recover([]string{tickJobID}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("job is %q, want ready", j.Status)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(string(m.Content), "lost") {
			found = true
		}
	}
	if !found {
		t.Errorf("no tool message containing %q in the log", "lost")
	}
}

// TestRecoverLiveOp: an op running with the PID of a live `sleep 1` is
// adopted; when the process exits the watcher reports it done and the
// job becomes ready — within 5 s.
func TestRecoverLiveOp(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Reap the process when it exits: an un-reaped zombie would still
	// answer kill(pid, 0) and the watcher would wait forever.
	go func() { _ = cmd.Wait() }()

	eng := fake.New(1)
	l, store := recoverSetup(t, eng, "call_1", "shell", agentjob.OpRunning, pid)
	if err := l.Recover([]string{tickJobID}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := store.Get(tickJobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == agentjob.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job is %q after 5 s, want ready", j.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRecoverPendingOpStarts: an op left pending is started again — the
// same startOp the tick uses: marked running, and the Runner receives
// the call with the job's workspace.
func TestRecoverPendingOpStarts(t *testing.T) {
	got := make(chan ops.Call, 1)
	eng := fake.New(1)
	l, store := recoverSetup(t, eng, "call_1", "shell", agentjob.OpPending, 0,
		captureTool{name: "shell", gotCh: got})
	if err := l.Recover([]string{tickJobID}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	jobOps, err := store.Ops(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobOps) != 1 || jobOps[0].Status != agentjob.OpRunning {
		t.Fatalf("ops = %+v, want the op running", jobOps)
	}
	select {
	case c := <-got:
		if c.OpID != "call_1" || c.Workspace != "/ws/"+tickJobID {
			t.Errorf("call = %+v, want call_1 in the job workspace", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the runner never received the call")
	}
}

// TestRecoverSubmitsReady: a job whose tool calls are all answered comes
// back ready through RecoverLoop and is submitted.
func TestRecoverSubmitsReady(t *testing.T) {
	eng := fake.New(1)
	l, store := recoverSetup(t, eng, "call_1", "shell", agentjob.OpDone, 0)
	answered, err := json.Marshal(toolContent{ToolCallID: "call_1", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessages(tickJobID, agentjob.LoopMessage{Role: "tool", Content: answered}); err != nil {
		t.Fatal(err)
	}
	if err := l.Recover([]string{tickJobID}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("job is %q, want ready", j.Status)
	}
	l.mu.Lock()
	ready := append([]string(nil), l.ready...)
	l.mu.Unlock()
	if len(ready) != 1 || ready[0] != tickJobID {
		t.Errorf("ready list = %v, want [%s]", ready, tickJobID)
	}
}
