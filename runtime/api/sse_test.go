package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

// newSSEServer builds a test server with a live Broker, redacting the
// given known values.
func newSSEServer(t *testing.T, values ...string) (*Server, *Broker) {
	t.Helper()
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	b := NewBroker(8)
	s := New(Deps{
		Store:    store,
		Redactor: redact.New(values),
		Token:    testToken,
		Version:  "test",
		Broker:   b,
	})
	return s, b
}

// readFirstEvent reads from the SSE body until the blank line that ends
// the first event and returns the whole frame (id/event/data lines and
// the trailing blank).
func readFirstEvent(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	var lines []string
	for {
		if !sc.Scan() {
			t.Fatalf("stream closed before the first event (%v)", sc.Err())
		}
		line := sc.Text()
		lines = append(lines, line)
		if line == "" {
			break
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestSSEStreamsEvent(t *testing.T) {
	s, b := newSSEServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	b.Publish(Event{Type: "op.done", JobID: "job-1", Data: json.RawMessage(`{"ok":true}`)})
	got := readFirstEvent(t, bufio.NewScanner(resp.Body))
	want := "id: 1\nevent: op.done\ndata: {\"ok\":true}\n\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestSSERedactsData(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	s, b := newSSEServer(t, secret)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	b.Publish(Event{Type: "op.progress", Data: json.RawMessage(`{"text":"key is ` + secret + `"}`)})
	got := readFirstEvent(t, bufio.NewScanner(resp.Body))
	if strings.Contains(got, secret) {
		t.Fatalf("raw secret in the stream: %q", got)
	}
	if !strings.Contains(got, `[redacted]`) {
		t.Fatalf("no [redacted] in the stream: %q", got)
	}
}

func TestSSEEndsOnClientCancel(t *testing.T) {
	s, b := newSSEServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	// Wait for the handler to have subscribed: it writes its headers
	// before it subscribes, so from this point on reading the recorder
	// is race-free.
	deadline := time.Now().Add(2 * time.Second)
	for b.subsLen() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	if resp := rec.Result(); resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.StatusCode)
	}
	// Now the client goes away; the stream must end with it.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return when the client went away")
	}
	if b.subsLen() != 0 {
		t.Fatal("the client's subscription was not cleaned up")
	}
}

func TestSSERequiresToken(t *testing.T) {
	s, _ := newSSEServer(t)
	rec := getNoToken(t, s.Handler(), "/v1/events")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
		t.Fatalf("body = %q, want %q", body, `{"error":"unauthorized"}`)
	}
}
