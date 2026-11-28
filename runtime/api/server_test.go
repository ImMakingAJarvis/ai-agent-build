package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

const testToken = "test-token-for-the-guard"

// newTestServer builds a Server over a fresh temp store, redacting the
// given known values.
func newTestServer(t *testing.T, values ...string) (*Server, *agentjob.Store) {
	t.Helper()
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(Deps{
		Store:    store,
		Redactor: redact.New(values),
		Token:    testToken,
		Version:  "test",
	}), store
}

// getWithToken runs GET path against the handler with the bearer token.
func getWithToken(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getNoToken runs GET path without any Authorization header.
func getNoToken(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestGetTask404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := getWithToken(t, s.Handler(), "/v1/tasks/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"error":"not found"}` {
		t.Fatalf("body = %q, want %q", body, `{"error":"not found"}`)
	}
}

func TestGetTaskReturnsStateAndOps(t *testing.T) {
	s, store := newTestServer(t)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "the job"}); err != nil {
		t.Fatal(err)
	}
	op := agentjob.LoopOp{ID: "op1", Tool: "shell", Status: agentjob.OpRunning}
	if err := store.PutOp("j1", op); err != nil {
		t.Fatal(err)
	}
	rec := getWithToken(t, s.Handler(), "/v1/tasks/j1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Job struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"job"`
		Ops []struct {
			ID     string `json:"id"`
			Tool   string `json:"tool"`
			Status string `json:"status"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if body.Job.ID != "j1" || body.Job.Title != "the job" || body.Job.Status != agentjob.Ready {
		t.Fatalf("job = %+v, want id j1, title the job, status ready", body.Job)
	}
	if len(body.Ops) != 1 || body.Ops[0].ID != "op1" ||
		body.Ops[0].Tool != "shell" || body.Ops[0].Status != agentjob.OpRunning {
		t.Fatalf("ops = %+v, want one running shell op op1", body.Ops)
	}
}

func TestMessagesRedacted(t *testing.T) {
	const secret = "super-secret-value-123"
	s, store := newTestServer(t, secret)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1"}); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal("the key is " + secret + " on the wire")
	if _, err := store.AppendMessages("j1", agentjob.LoopMessage{
		Role: "tool", Content: content,
	}); err != nil {
		t.Fatal(err)
	}
	rec := getWithToken(t, s.Handler(), "/v1/tasks/j1/messages")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("response contains the secret: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("response does not carry the redaction placeholder: %s", body)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("not a JSON array: %v (body %s)", err, body)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
}

func TestStateCountsJobsByStatus(t *testing.T) {
	s, store := newTestServer(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := store.Ingest(agentjob.Spec{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	for id, st := range map[string]string{
		"a": agentjob.Running,
		"b": agentjob.Done,
		"c": agentjob.Parked,
	} {
		if err := store.SetStatus(id, agentjob.State{Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	rec := getWithToken(t, s.Handler(), "/v1/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Draining   bool             `json:"draining"`
		RunningOps []map[string]any `json:"running_ops"`
		Jobs       map[string]int   `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if body.Draining {
		t.Error("draining = true, want false before card m4-1e")
	}
	want := map[string]int{
		agentjob.Ready: 1, agentjob.Running: 1, agentjob.Done: 1, agentjob.Parked: 1,
	}
	if len(body.Jobs) != len(want) {
		t.Fatalf("jobs = %v, want %v", body.Jobs, want)
	}
	for st, n := range want {
		if body.Jobs[st] != n {
			t.Fatalf("jobs[%q] = %d, want %d (all %v)", st, body.Jobs[st], n, body.Jobs)
		}
	}
	if len(body.RunningOps) != 0 {
		t.Fatalf("running_ops = %v, want empty", body.RunningOps)
	}
}

// slowTool blocks until its context is done, so its op stays running for
// the length of the test.
type slowTool struct{}

func (slowTool) Spec() ops.Spec { return ops.Spec{Name: "slow", Class: ops.Long} }

func (slowTool) Run(ctx context.Context, c ops.Call, progress func()) ops.Result {
	<-ctx.Done()
	return ops.Result{Status: agentjob.OpCancelled}
}

func TestStateListsRunningOps(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Ingest(agentjob.Spec{ID: "j1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp("j1", agentjob.LoopOp{ID: "op1", Tool: "slow", Status: agentjob.OpRunning}); err != nil {
		t.Fatal(err)
	}
	runner := &ops.Runner{Registry: ops.NewRegistry(slowTool{})}
	if err := runner.Start(ops.Call{Name: "slow", JobID: "j1", OpID: "op1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Cancel("op1") })

	s := New(Deps{Store: store, Runner: runner, Token: testToken, Version: "test"})
	rec := getWithToken(t, s.Handler(), "/v1/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		RunningOps []map[string]string `json:"running_ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if len(body.RunningOps) != 1 ||
		body.RunningOps[0]["job"] != "j1" || body.RunningOps[0]["op"] != "op1" {
		t.Fatalf("running_ops = %v, want one entry job j1 op op1", body.RunningOps)
	}
}

func TestHealthzNoToken(t *testing.T) {
	s, _ := newTestServer(t)
	rec := getNoToken(t, s.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestV1RoutesRequireToken(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"/v1/state", "/v1/tasks/j1", "/v1/tasks/j1/messages"} {
		rec := getNoToken(t, s.Handler(), path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: code = %d, want 401 (body %s)", path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
			t.Fatalf("%s without token: body = %q, want the guard's 401 body", path, body)
		}
	}
	// A wrong token is refused the same way.
	req := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	// The right token gets through.
	if rec := getWithToken(t, s.Handler(), "/v1/state"); rec.Code != http.StatusOK {
		t.Fatalf("right token: code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}
