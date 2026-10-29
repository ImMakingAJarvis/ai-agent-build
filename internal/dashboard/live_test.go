package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// sseWait polls the recorder until the SSE output contains want.
func sseWait(t *testing.T, rec *httptest.ResponseRecorder, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("SSE output lacks %q:\n%s", want, rec.Body.String())
}

func TestJobLiveStream(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	dir := t.TempDir()
	st, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: "live-1", Title: "Live job"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("live-1", agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	key := agentjob.StreamKey("live-1")
	seed := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"plan it"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"hi"}]}}`,
	}
	for _, l := range seed {
		if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": l}}).Err(); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl"), redis: rdb}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs/{id}/live", s.handleJobLive)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := httptest.NewRecorder()
	go mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/live-1/live", nil).WithContext(reqCtx))

	// Meta and the seed of everything published before the connection.
	sseWait(t, rec, `"live":true`, 3*time.Second)
	sseWait(t, rec, `"kind":"thought"`, 3*time.Second)
	sseWait(t, rec, `"body":"plan it"`, 3*time.Second)
	sseWait(t, rec, `"kind":"command"`, 3*time.Second)
	sseWait(t, rec, `"kind":"result"`, 3*time.Second)
	sseWait(t, rec, `"body":"hi"`, 3*time.Second)

	// A new line published while connected arrives as a step event.
	step := `{"type":"assistant","message":{"content":[{"type":"text","text":"all done"}]}}`
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": step}}).Err(); err != nil {
		t.Fatal(err)
	}
	sseWait(t, rec, "event: step", 8*time.Second)
	sseWait(t, rec, `"body":"all done"`, 8*time.Second)

	// The job finishing closes the stream with a done event.
	if err := st.SetStatus("live-1", agentjob.State{Status: agentjob.Done}); err != nil {
		t.Fatal(err)
	}
	sseWait(t, rec, "event: done", 10*time.Second)
	sseWait(t, rec, `"status":"done"`, 10*time.Second)
}

func TestJobLiveFileFallback(t *testing.T) {
	dir := t.TempDir()
	st, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: "fb-1", Title: "File fallback"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("fb-1", agentjob.State{Status: agentjob.Running}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFile("fb-1", "transcript.txt", []byte(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n")); err != nil {
		t.Fatal(err)
	}

	// No redis client: the endpoint must fall back to tailing the file.
	s := &Server{historyPath: filepath.Join(dir, "jobs.jsonl")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs/{id}/live", s.handleJobLive)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := httptest.NewRecorder()
	go mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/jobs/fb-1/live", nil).WithContext(reqCtx))

	sseWait(t, rec, `"body":"hello"`, 3*time.Second)

	// A new line appended to the transcript shows up on the next tail tick.
	f, err := os.OpenFile(jobLogPath(st, "fb-1"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	sseWait(t, rec, "event: step", 10*time.Second)
	sseWait(t, rec, `"body":"ok"`, 10*time.Second)

	if err := st.SetStatus("fb-1", agentjob.State{Status: agentjob.Done}); err != nil {
		t.Fatal(err)
	}
	sseWait(t, rec, "event: done", 12*time.Second)
}
