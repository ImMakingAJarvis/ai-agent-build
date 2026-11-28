package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeToken writes a token file the subcommand can read (a trailing
// newline, like one written with echo).
func writeToken(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newDrainServer serves POST /v1/drain with the given running ops JSON,
// answers GET /v1/state from bodies (poll n gets bodies[n], the last
// body repeats), and records the Authorization header of the last
// request in *auth.
func newDrainServer(t *testing.T, auth *string, drainOps string, bodies ...string) *httptest.Server {
	t.Helper()
	var got int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/drain":
			w.Write([]byte(`{"draining":true,"running_ops":` + drainOps + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/state" && len(bodies) > 0:
			i := int(atomic.AddInt32(&got, 1)) - 1
			if i >= len(bodies) {
				i = len(bodies) - 1
			}
			w.Write([]byte(bodies[i]))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

const opsJSON = `[{"job":"task-a","op":"accept-1"},{"job":"task-b","op":"shell-2"}]`

// TestDrainPrintsRunningOps: the drain subcommand posts /v1/drain and
// prints the running ops as job/op pairs; without --wait it returns 0 at
// once.
func TestDrainPrintsRunningOps(t *testing.T) {
	var auth string
	srv := newDrainServer(t, &auth, opsJSON)
	defer srv.Close()

	var out strings.Builder
	code := drain([]string{"drain", "--addr", srv.URL, "--token-file", writeToken(t, "sekrit")}, srv.Client(), &out)
	if code != 0 {
		t.Fatalf("drain exited %d, want 0:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "task-a/accept-1") || !strings.Contains(s, "task-b/shell-2") {
		t.Errorf("running ops not printed job/op:\n%s", s)
	}
}

// TestDrainWaitReturnsZeroWhenIdle: with --wait, the subcommand polls
// GET /v1/state until running_ops is empty and then returns 0.
func TestDrainWaitReturnsZeroWhenIdle(t *testing.T) {
	busy := `{"draining":true,"running_ops":[{"job":"task-a","op":"accept-1"}]}`
	idle := `{"draining":true,"running_ops":[]}`
	var auth string
	srv := newDrainServer(t, &auth, opsJSON, busy, idle) // first poll busy, then idle
	defer srv.Close()

	old := drainPoll
	drainPoll = 5 * time.Millisecond
	defer func() { drainPoll = old }()

	var out strings.Builder
	code := drain([]string{"drain", "--addr", srv.URL, "--token-file", writeToken(t, "sekrit"), "--wait", "5s"}, srv.Client(), &out)
	if code != 0 {
		t.Fatalf("drain --wait exited %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "drained") {
		t.Errorf("no drained line:\n%s", out.String())
	}
}

// TestDrainWaitExit1WhenStillRunning: when the running set is not empty
// at the --wait deadline the subcommand exits 1 and names the ops still
// running.
func TestDrainWaitExit1WhenStillRunning(t *testing.T) {
	busy := `{"draining":true,"running_ops":[{"job":"task-a","op":"accept-1"}]}`
	var auth string
	srv := newDrainServer(t, &auth, `[{"job":"task-a","op":"accept-1"}]`, busy) // busy on every poll
	defer srv.Close()

	old := drainPoll
	drainPoll = 2 * time.Millisecond
	defer func() { drainPoll = old }()

	var out strings.Builder
	code := drain([]string{"drain", "--addr", srv.URL, "--token-file", writeToken(t, "sekrit"), "--wait", "40ms"}, srv.Client(), &out)
	if code != 1 {
		t.Fatalf("drain --wait exited %d, want 1:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "still running") {
		t.Errorf("no still-running line:\n%s", s)
	}
	if !strings.Contains(s, "task-a/accept-1") {
		t.Errorf("the op still running is not named:\n%s", s)
	}
}

// TestDrainSendsBearerToken: the token file's content goes on the wire
// as the Bearer header of the drain request (a trailing newline trimmed
// off).
func TestDrainSendsBearerToken(t *testing.T) {
	var auth string
	srv := newDrainServer(t, &auth, `[]`)
	defer srv.Close()

	var out strings.Builder
	code := drain([]string{"drain", "--addr", srv.URL, "--token-file", writeToken(t, "sekrit")}, srv.Client(), &out)
	if code != 0 {
		t.Fatalf("drain exited %d, want 0:\n%s", code, out.String())
	}
	if auth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer sekrit")
	}
}
