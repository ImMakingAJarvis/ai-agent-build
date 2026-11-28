// approvals_test.go covers the approval endpoints (card ELR-109,
// M4.3d): the owner's decision is recorded, acted on through the loop
// and put on the event stream; lapsed, mismatched and non-owner
// decisions are refused; the list serves the approvals with the
// request text redacted.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

// decision is the body the approval tests POST.
type decision struct {
	ApprovalID string `json:"approval_id"`
	Hash       string `json:"hash"`
	Decision   string `json:"decision"`
	Note       string `json:"note"`
	DecidedBy  string `json:"decided_by"`
}

// gateTool holds its op open until its release is closed: the approve
// test asserts the op running, then lets it finish through the runner.
type gateTool struct{ release chan struct{} }

func (gateTool) Spec() ops.Spec { return ops.Spec{Name: "shell", Class: ops.Quick} }
func (g gateTool) Run(ctx context.Context, _ ops.Call, _ func()) ops.Result {
	select {
	case <-g.release:
		return ops.Result{Status: agentjob.OpDone, Output: "ok"}
	case <-ctx.Done():
		return ops.Result{}
	}
}

// parkApproval puts the store in the state parkForApproval (card
// ELR-107) leaves it in: the job ingested, the op awaiting the
// approval, the pending approval with the hash of the exact call, and
// the job waiting on the approval.
func parkApproval(t *testing.T, store *agentjob.Store, jobID, opID, tool string, args json.RawMessage) {
	t.Helper()
	now := time.Now()
	if _, err := store.Ingest(agentjob.Spec{ID: jobID, Title: "t", Instructions: "i", Driver: "loop"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp(jobID, agentjob.LoopOp{
		ID: opID, ToolCallID: "call_1", Tool: tool, Args: args,
		Status: agentjob.OpAwaitingApproval,
	}); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}{tool, args})
	if err != nil {
		t.Fatal(err)
	}
	remindAt := now.Add(time.Minute)
	expiresAt := now.Add(24 * time.Hour)
	if err := store.PutApproval(jobID, agentjob.LoopApproval{
		ID: opID, OpID: opID, Request: req,
		Status:    agentjob.ApprovalPending,
		Hash:      agentjob.ApprovalHash(tool, args),
		RemindAt:  &remindAt,
		ExpiresAt: &expiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := store.Get(jobID)
	if err != nil {
		t.Fatal(err)
	}
	st := job.State
	st.Status = agentjob.Waiting
	st.WaitReason = agentjob.WaitApproval
	if err := store.SetStatus(jobID, st); err != nil {
		t.Fatal(err)
	}
}

// newApprovalLoop wires a real loop over the store: the approve path
// (startApprovedOp) and the deny path (HandleOpDone) touch only the
// store, the runner and the in-memory ready queue, so no engine or
// slots are needed.
func newApprovalLoop(t *testing.T, store *agentjob.Store, tools ...ops.Tool) *loop.Loop {
	t.Helper()
	reg := ops.NewRegistry(tools...)
	runner := &ops.Runner{
		Registry: reg,
		Timeouts: map[ops.Class]time.Duration{ops.Quick: time.Minute, ops.Normal: time.Minute},
	}
	l := loop.New(loop.Deps{
		Runner:   runner,
		Registry: reg,
		Store:    store,
		Config:   config.Config{SpillBytes: 1 << 20},
		Now:      time.Now,
	})
	runner.OnDone = func(jobID, opID string, res ops.Result) {
		_ = l.HandleOpDone(jobID, opID, res)
	}
	return l
}

// newApprovalServer builds a Server over the store with the loop wired
// as the decision actor and the given owner Discord id.
func newApprovalServer(t *testing.T, store *agentjob.Store, l *loop.Loop, owner string) *Server {
	t.Helper()
	return New(Deps{
		Store:             store,
		Redactor:          redact.New(nil),
		Token:             testToken,
		Version:           "test",
		OwnerDiscordID:    owner,
		OnApprovalDecided: l.HandleApprovalDecided,
	})
}

// postDecision POSTs d to j1's approvals route and returns the
// response.
func postDecision(t *testing.T, h http.Handler, d decision) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return postWithToken(t, h, "/v1/tasks/j1/approvals", string(b))
}

// changedOf pulls the "changed" flag out of a 200 approvals response.
func changedOf(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	return out.Changed
}

// waitForOpStatus polls the store until the op reaches want.
func waitForOpStatus(t *testing.T, store *agentjob.Store, jobID, opID, want string) agentjob.LoopOp {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		op, err := store.GetOp(jobID, opID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status == want {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("op %s status = %s, want %s", opID, op.Status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestApproveStartsOp: an approved decision starts the parked op through
// the real loop — the op is running, the job waits on the op instead of
// the approval, the op runs to its end — and approval.decided lands on
// the event stream.
func TestApproveStartsOp(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"ls"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	gate := &gateTool{release: make(chan struct{})}
	l := newApprovalLoop(t, store, gate)
	broker := NewBroker(8)
	ch, cancel := broker.Subscribe()
	defer cancel()
	s := New(Deps{
		Store:             store,
		Redactor:          redact.New(nil),
		Token:             testToken,
		Version:           "test",
		Broker:            broker,
		OnApprovalDecided: l.HandleApprovalDecided,
	})
	rec := postDecision(t, s.Handler(), decision{
		ApprovalID: "op1",
		Hash:       agentjob.ApprovalHash("shell", args),
		Decision:   "approve",
	})
	if !changedOf(t, rec) {
		t.Fatal("changed = false, want true")
	}
	// startApprovedOp recorded the op as running before the POST
	// returned; the job now waits on the op, not the approval.
	waitForOpStatus(t, store, "j1", "op1", agentjob.OpRunning)
	job, err := store.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != agentjob.Waiting || job.WaitReason != agentjob.WaitOperation {
		t.Errorf("job = %s/%s, want waiting on the operation", job.Status, job.WaitReason)
	}
	close(gate.release)
	op := waitForOpStatus(t, store, "j1", "op1", agentjob.OpDone)
	if !strings.Contains(string(op.Result), "ok") {
		t.Errorf("op result = %s, want the tool's output", op.Result)
	}
	select {
	case ev := <-ch:
		if ev.Type != "approval.decided" || ev.JobID != "j1" || ev.OpID != "op1" {
			t.Fatalf("event = %+v, want approval.decided for j1/op1", ev)
		}
		var d struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("event data = %s: %v", ev.Data, err)
		}
		if d.Status != agentjob.ApprovalApproved {
			t.Errorf("event status = %q, want %q", d.Status, agentjob.ApprovalApproved)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval.decided never reached the event stream")
	}
}

// TestDenyReturnsNote: a denied decision finishes the op as a failed op
// whose result carries "denied: <the note>", and the job goes back to
// ready.
func TestDenyReturnsNote(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"rm -rf /"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	rec := postDecision(t, s.Handler(), decision{
		ApprovalID: "op1",
		Hash:       agentjob.ApprovalHash("shell", args),
		Decision:   "deny",
		Note:       "not in this round",
	})
	if !changedOf(t, rec) {
		t.Fatal("changed = false, want true")
	}
	op := waitForOpStatus(t, store, "j1", "op1", agentjob.OpFailed)
	if !strings.Contains(string(op.Result), "denied: not in this round") {
		t.Errorf("op result = %s, want it to carry the denial note", op.Result)
	}
	job, err := store.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != agentjob.Ready {
		t.Errorf("job status = %q, want %s", job.Status, agentjob.Ready)
	}
}

// TestHashMismatch400: a decision whose hash is not the approval's is
// refused and changes nothing.
func TestHashMismatch400(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	parkApproval(t, store, "j1", "op1", "shell", json.RawMessage(`{"cmd":"ls"}`))
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	rec := postDecision(t, s.Handler(), decision{ApprovalID: "op1", Hash: "deadbeef", Decision: "approve"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	aps, err := store.Approvals("j1")
	if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalPending {
		t.Fatalf("approvals = %+v (err %v), want still pending", aps, err)
	}
}

// TestDecisionAfterCancelIs409AndChangesNothing: the job was cancelled
// (parked) — the approval has lapsed, the decision is 409 and the
// approval and op are untouched.
func TestDecisionAfterCancelIs409AndChangesNothing(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"ls"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	job, err := store.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	st := job.State
	st.Status = agentjob.Parked
	if err := store.SetStatus("j1", st); err != nil {
		t.Fatal(err)
	}
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	rec := postDecision(t, s.Handler(), decision{
		ApprovalID: "op1",
		Hash:       agentjob.ApprovalHash("shell", args),
		Decision:   "approve",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	aps, err := store.Approvals("j1")
	if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalPending {
		t.Fatalf("approvals = %+v (err %v), want still pending", aps, err)
	}
	op, err := store.GetOp("j1", "op1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != agentjob.OpAwaitingApproval {
		t.Errorf("op status = %q, want %s", op.Status, agentjob.OpAwaitingApproval)
	}
}

// TestNonOwnerDiscordID403: a decided_by that names a Discord id other
// than the configured owner is 403 and changes nothing — the bearer
// token is the owner identity, and it cannot decide as someone else.
func TestNonOwnerDiscordID403(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"ls"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "1234567890")
	rec := postDecision(t, s.Handler(), decision{
		ApprovalID: "op1",
		Hash:       agentjob.ApprovalHash("shell", args),
		Decision:   "approve",
		DecidedBy:  "discord:9999999999",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	aps, err := store.Approvals("j1")
	if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalPending {
		t.Fatalf("approvals = %+v (err %v), want still pending", aps, err)
	}
}

// TestOwnerDiscordIDAccepted: a decided_by in the discord:<id> form
// naming the configured owner is accepted and recorded.
func TestOwnerDiscordIDAccepted(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"ls"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "1234567890")
	rec := postDecision(t, s.Handler(), decision{
		ApprovalID: "op1",
		Hash:       agentjob.ApprovalHash("shell", args),
		Decision:   "approve",
		DecidedBy:  "discord:1234567890",
	})
	if !changedOf(t, rec) {
		t.Fatal("changed = false, want true")
	}
	aps, err := store.Approvals("j1")
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals = %+v (err %v), want 1", aps, err)
	}
	if aps[0].DecidedBy != "discord:1234567890" {
		t.Errorf("decided_by = %q, want discord:1234567890", aps[0].DecidedBy)
	}
}

// TestSecondDecisionIsNoop200: a decision for an approval that already
// carries one is 200 {"changed":false} and the first decision stands.
func TestSecondDecisionIsNoop200(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	args := json.RawMessage(`{"cmd":"ls"}`)
	parkApproval(t, store, "j1", "op1", "shell", args)
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	hash := agentjob.ApprovalHash("shell", args)
	if !changedOf(t, postDecision(t, s.Handler(), decision{ApprovalID: "op1", Hash: hash, Decision: "approve", DecidedBy: "first"})) {
		t.Fatal("first decision changed = false, want true")
	}
	if changedOf(t, postDecision(t, s.Handler(), decision{ApprovalID: "op1", Hash: hash, Decision: "deny", DecidedBy: "second"})) {
		t.Fatal("second decision changed = true, want false")
	}
	aps, err := store.Approvals("j1")
	if err != nil || len(aps) != 1 || aps[0].Status != agentjob.ApprovalApproved || aps[0].DecidedBy != "first" {
		t.Fatalf("approvals = %+v (err %v), want the first decision untouched", aps, err)
	}
}

// TestUnknownApproval404: a decision for an approval the job does not
// have is 404.
func TestUnknownApproval404(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "t", Instructions: "i", Driver: "loop"}); err != nil {
		t.Fatal(err)
	}
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	rec := postDecision(t, s.Handler(), decision{ApprovalID: "nope", Hash: "h", Decision: "approve"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestApprovalsRequireToken: the approval routes are behind the bearer
// guard like every other /v1 route.
func TestApprovalsRequireToken(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	parkApproval(t, store, "j1", "op1", "shell", json.RawMessage(`{"cmd":"ls"}`))
	s := newApprovalServer(t, store, newApprovalLoop(t, store, &gateTool{release: make(chan struct{})}), "")
	rec := postNoToken(t, s.Handler(), "/v1/tasks/j1/approvals", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST code = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
		t.Fatalf("POST body = %q, want %q", body, `{"error":"unauthorized"}`)
	}
	rec = getNoToken(t, s.Handler(), "/v1/tasks/j1/approvals")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET code = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestListApprovalsRedacted: GET /v1/tasks/{id}/approvals serves the
// approvals with the request text through the Redactor — a known value
// in the call's args comes back scrubbed — and an empty array for a
// job with no approvals.
func TestListApprovalsRedacted(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	secret := "hunter2-secret-value"
	parkApproval(t, store, "j1", "op1", "shell", json.RawMessage(`{"cmd":"curl https://x/?token=`+secret+`"}`))
	s := New(Deps{
		Store:          store,
		Redactor:       redact.New([]string{secret}),
		Token:          testToken,
		Version:        "test",
		OwnerDiscordID: "",
	})
	rec := getWithToken(t, s.Handler(), "/v1/tasks/j1/approvals")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("body carries the secret unredacted: %s", body)
	}
	if !strings.Contains(body, `"id":"op1"`) {
		t.Fatalf("body = %s, want the approval listed", body)
	}
	if _, err := store.Ingest(agentjob.Spec{ID: "j2", Title: "t", Instructions: "i", Driver: "loop"}); err != nil {
		t.Fatal(err)
	}
	rec = getWithToken(t, s.Handler(), "/v1/tasks/j2/approvals")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `[]` {
		t.Fatalf("body = %q, want []", body)
	}
}
