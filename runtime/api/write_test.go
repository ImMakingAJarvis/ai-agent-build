package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

// postWithToken POSTs body to path with the bearer token.
func postWithToken(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postNoToken POSTs body to path without any Authorization header.
func postNoToken(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// hangTool blocks until its context is done: the shape of a running tool
// call that a cancel has to stop.
type hangTool struct{}

func (hangTool) Spec() ops.Spec { return ops.Spec{Name: "hang", Class: ops.Normal} }
func (hangTool) Run(ctx context.Context, c ops.Call, progress func()) ops.Result {
	<-ctx.Done()
	return ops.Result{}
}

// waitRunningOp polls until opID appears in the runner's live ops.
func waitRunningOp(t *testing.T, r *ops.Runner, opID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, id := range r.Running() {
			if id == opID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("op %s never showed as running", opID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateTask201(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var mu sync.Mutex
	var submitted []string
	s := New(Deps{
		Store:    store,
		Redactor: redact.New(nil),
		Token:    testToken,
		Version:  "test",
		Submitter: func(jobID string) error {
			mu.Lock()
			submitted = append(submitted, jobID)
			mu.Unlock()
			return nil
		},
	})
	rec := postWithToken(t, s.Handler(), "/v1/tasks",
		`{"title":"the task","instructions":"do the thing","branch":"b","acceptance":["go test ./..."]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if body.ID == "" {
		t.Fatalf("id empty (body %s)", rec.Body.String())
	}
	job, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("job not in the store: %v", err)
	}
	if job.Title != "the task" || job.Instructions != "do the thing" ||
		job.Branch != "b" || job.Driver != "loop" || job.Status != agentjob.Ready ||
		len(job.Acceptance) != 1 || job.Acceptance[0] != "go test ./..." {
		t.Fatalf("job = %+v, want the created task, ready", job)
	}
	mu.Lock()
	got := append([]string(nil), submitted...)
	mu.Unlock()
	if len(got) != 1 || got[0] != body.ID {
		t.Fatalf("submitted = %v, want [%s]", got, body.ID)
	}
}

func TestCreateTaskEmptyInstructions400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postWithToken(t, s.Handler(), "/v1/tasks", `{"title":"t","instructions":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateTaskBadJSON400(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postWithToken(t, s.Handler(), "/v1/tasks", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateTaskConflict409(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := New(Deps{
		Store: store,
		Token: testToken,
		Submit: func(TaskRequest) (string, error) {
			return "", fmt.Errorf("task already exists: %w", ErrConflict)
		},
	})
	rec := postWithToken(t, s.Handler(), "/v1/tasks", `{"instructions":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCancelStopsRunningOps(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "j", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus("j1", agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutOp("j1", agentjob.LoopOp{ID: "op1", Tool: "hang", Status: agentjob.OpRunning}); err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan ops.Result, 1)
	runner := &ops.Runner{
		Registry: ops.NewRegistry(hangTool{}),
		Timeouts: map[ops.Class]time.Duration{ops.Normal: 10 * time.Second},
		OnDone:   func(jobID, opID string, res ops.Result) { doneCh <- res },
	}
	if err := runner.Start(ops.Call{JobID: "j1", OpID: "op1", Name: "hang"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunningOp(t, runner, "op1")

	s := New(Deps{Store: store, Runner: runner, Token: testToken})
	rec := postWithToken(t, s.Handler(), "/v1/tasks/j1/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	select {
	case res := <-doneCh:
		if res.Status != agentjob.OpCancelled {
			t.Fatalf("op result = %+v, want status %q", res, agentjob.OpCancelled)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not arrive after the cancel")
	}
	job, err := store.Get("j1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != agentjob.Parked {
		t.Fatalf("job status = %q, want %q (stopped is the store's parked status)", job.Status, agentjob.Parked)
	}
}

func TestCancelTwiceIsNoop(t *testing.T) {
	s, store := newTestServer(t)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "j", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec := postWithToken(t, s.Handler(), "/v1/tasks/j1/cancel", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel %d: code = %d, want 200 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
	job, err := store.Get("j1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != agentjob.Parked {
		t.Fatalf("job status = %q, want %q", job.Status, agentjob.Parked)
	}
}

func TestCancelUnknown404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postWithToken(t, s.Handler(), "/v1/tasks/nope/cancel", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"error":"not found"}` {
		t.Fatalf("body = %q, want %q", body, `{"error":"not found"}`)
	}
}

func TestMessageAppended(t *testing.T) {
	s, store := newTestServer(t)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "j", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	rec := postWithToken(t, s.Handler(), "/v1/tasks/j1/messages", `{"text":"please hurry"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	msgs, err := store.Messages("j1")
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("role = %q, want %q", msgs[0].Role, "user")
	}
	var text string
	if err := json.Unmarshal(msgs[0].Content, &text); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if text != "please hurry" {
		t.Fatalf("content = %q, want %q", text, "please hurry")
	}
}

func TestMessageEmpty400(t *testing.T) {
	s, store := newTestServer(t)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "j", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	rec := postWithToken(t, s.Handler(), "/v1/tasks/j1/messages", `{"text":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestWritesRequireToken(t *testing.T) {
	s, store := newTestServer(t)
	if _, err := store.Ingest(agentjob.Spec{ID: "j1", Title: "j", Instructions: "i"}); err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		path string
		body string
	}{
		{"/v1/tasks", `{"instructions":"x"}`},
		{"/v1/tasks/j1/cancel", ""},
		{"/v1/tasks/j1/messages", `{"text":"x"}`},
		{"/v1/tasks/j1/approvals", "{}"},
	}
	for _, c := range calls {
		rec := postNoToken(t, s.Handler(), c.path, c.body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: code = %d, want 401 (body %s)", c.path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
			t.Fatalf("%s: body = %q, want %q", c.path, body, `{"error":"unauthorized"}`)
		}
	}
}
