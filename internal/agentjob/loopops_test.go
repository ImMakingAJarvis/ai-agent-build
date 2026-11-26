package agentjob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPutGetOp: an op round-trips through <JobDir>/ops/<id>.json with
// every field intact, the file lands where the job expects it, and a
// missing op is an error.
func TestPutGetOp(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	deadline := now.Add(30 * time.Second)
	op := LoopOp{
		ID: "op-1", ToolCallID: "call_abc", Tool: "bash",
		Args: json.RawMessage(`{"cmd":"ls -la"}`),
		Status: OpRunning, PID: 4242,
		StartedAt: &now, Deadline: &deadline,
	}
	if err := s.PutOp("job1", op); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.JobDir("job1"), "ops", "op-1.json")); err != nil {
		t.Fatalf("op file not written: %v", err)
	}
	got, err := s.GetOp("job1", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "op-1" || got.ToolCallID != "call_abc" || got.Tool != "bash" {
		t.Errorf("identity = %+v", got)
	}
	// writeJSON indents, so compare the args by content, not raw bytes.
	var args map[string]string
	if err := json.Unmarshal(got.Args, &args); err != nil {
		t.Fatalf("args do not parse: %v (%s)", err, got.Args)
	}
	if args["cmd"] != "ls -la" {
		t.Errorf("args = %s", got.Args)
	}
	if got.Status != OpRunning || got.PID != 4242 {
		t.Errorf("status=%q pid=%d", got.Status, got.PID)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(now) ||
		got.Deadline == nil || !got.Deadline.Equal(deadline) {
		t.Errorf("times = %v / %v", got.StartedAt, got.Deadline)
	}
	if _, err := s.GetOp("job1", "nope"); err == nil {
		t.Error("GetOp of a missing op should fail")
	}
}

// TestOpsSortedAndSkipsLogs: Ops returns the job's ops sorted by ID no
// matter the write order, skips .log and .tmp files, and a job without an
// ops directory returns nil, nil.
func TestOpsSortedAndSkipsLogs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c-op", "a-op", "b-op"} {
		if err := s.PutOp("job2", LoopOp{ID: id, Status: OpPending}); err != nil {
			t.Fatal(err)
		}
	}
	opsDir := filepath.Join(s.JobDir("job2"), "ops")
	if err := os.WriteFile(s.OpLogPath("job2", "a-op"), []byte("stream noise"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opsDir, "c-op.json.tmp"), []byte(`{"partial"`), 0o644); err != nil {
		t.Fatal(err)
	}
	ops, err := s.Ops("job2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("Ops returned %d ops, want 3: %+v", len(ops), ops)
	}
	for i, want := range []string{"a-op", "b-op", "c-op"} {
		if ops[i].ID != want {
			t.Errorf("ops[%d].ID = %q, want %q", i, ops[i].ID, want)
		}
	}
	// A fresh job has no ops directory yet.
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ops, err := s2.Ops("job3"); err != nil || ops != nil {
		t.Errorf("Ops of a job without an ops dir = %v, %v; want nil, nil", ops, err)
	}
}

// TestOpFinished: table over every status — the five terminal ones are
// finished, the other three are not.
func TestOpFinished(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{OpPending, false},
		{OpAwaitingApproval, false},
		{OpRunning, false},
		{OpDone, true},
		{OpFailed, true},
		{OpTimeout, true},
		{OpCancelled, true},
		{OpLost, true},
	}
	for _, c := range cases {
		if got := (LoopOp{Status: c.status}).Finished(); got != c.want {
			t.Errorf("Finished(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// TestPutApprovalRoundTrip: an approval round-trips through
// <JobDir>/approvals/<id>.json and comes back via Approvals.
func TestPutApprovalRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remind := time.Now().UTC().Truncate(time.Second)
	a := LoopApproval{
		ID: "ap-1", OpID: "op-1",
		Request:  json.RawMessage(`{"tool":"rm","arg":"-rf /tmp/x"}`),
		Status:   "pending",
		RemindAt: &remind,
	}
	if err := s.PutApproval("job1", a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.JobDir("job1"), "approvals", "ap-1.json")); err != nil {
		t.Fatalf("approval file not written: %v", err)
	}
	got, err := s.Approvals("job1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Approvals returned %d, want 1: %+v", len(got), got)
	}
	if got[0].ID != "ap-1" || got[0].OpID != "op-1" || got[0].Status != "pending" {
		t.Errorf("approval = %+v", got[0])
	}
	var req map[string]string
	if err := json.Unmarshal(got[0].Request, &req); err != nil {
		t.Fatalf("request does not parse: %v (%s)", err, got[0].Request)
	}
	if req["tool"] != "rm" || req["arg"] != "-rf /tmp/x" {
		t.Errorf("request = %s", got[0].Request)
	}
	if got[0].RemindAt == nil || !got[0].RemindAt.Equal(remind) {
		t.Errorf("remind_at = %v, want %v", got[0].RemindAt, remind)
	}
	if got[0].DecidedBy != "" || got[0].DecidedAt != nil {
		t.Errorf("unset decision fields = %+v", got[0])
	}
}

// TestOpBadID: empty ids and ids carrying '/' or '..' are rejected by the
// Put/Get paths for ops and approvals alike.
func TestOpBadID(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"", "a/b", "..", "a..b", "ops/../x"}
	for _, id := range bad {
		if err := s.PutOp("job1", LoopOp{ID: id}); err == nil {
			t.Errorf("PutOp accepted bad id %q", id)
		}
		if _, err := s.GetOp("job1", id); err == nil {
			t.Errorf("GetOp accepted bad id %q", id)
		}
		if err := s.PutApproval("job1", LoopApproval{ID: id}); err == nil {
			t.Errorf("PutApproval accepted bad id %q", id)
		}
	}
}
