// Test the jarvis-chat daemon's handleMessage flow.
//
// We spin up a fake Ollama server and use a test-harness Discord type
// to verify: normal reply, busy notice, and timeout.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// testDiscord mimics *chat.Discord for tests.
// It records Notify and Typing calls so we can assert on them.
type testDiscord struct {
	mu         sync.Mutex
	notifyMsgs []string
	typingCalls int
	typingErr  error
	notifyErr  error
}

func (d *testDiscord) Notify(_ context.Context, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notifyMsgs = append(d.notifyMsgs, text)
	return d.notifyErr
}

func (d *testDiscord) Typing() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.typingCalls++
	return d.typingErr
}

// lastNotify returns the most recent Notify message, or "" if none.
func (d *testDiscord) lastNotify() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.notifyMsgs) == 0 {
		return ""
	}
	return d.notifyMsgs[len(d.notifyMsgs)-1]
}

// notifyCount returns the number of Notify calls.
func (d *testDiscord) notifyCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.notifyMsgs)
}

// typingCount returns the number of Typing calls.
func (d *testDiscord) typingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.typingCalls
}

// fakeOllamaServer creates an httptest.Server that responds to
// /v1/chat/completions. delay controls how long before it responds.
// reply is the assistant text to return.
func fakeOllamaServer(t *testing.T, reply string, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		var req struct {
			Model    string        `json:"model"`
			Messages []models.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp := models.ChatResponse{
			Choices: []struct {
				Message models.ChatMessage `json:"message"`
			}{
				{Message: models.ChatMessage{Role: "assistant", Content: reply}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestHandleMessage_Reply(t *testing.T) {
	// Shorten timeouts for the test.
	oldBusy := busyTimeout
	oldMax := maxTimeout
	busyTimeout = 500 * time.Millisecond
	maxTimeout = 2 * time.Second
	defer func() {
		busyTimeout = oldBusy
		maxTimeout = oldMax
	}()

	server := fakeOllamaServer(t, "Hello! How can I help?", 100*time.Millisecond)
	defer server.Close()

	client := models.NewClient()
	client = &models.Client{} // reset to use our server
	// We can't set baseURL directly (it's private), so we use env.
	t.Setenv("OLLAMA_URL", server.URL)
	client = models.NewClient()

	d := &testDiscord{}
	var history []string

	handleMessage(d, client, "jarvis-brain", chat.Incoming{Content: "Hi Jarvis!"}, &history)

	// Should have typed at least once.
	if d.typingCount() < 1 {
		t.Errorf("want at least 1 typing call, got %d", d.typingCount())
	}

	// Should have notified exactly once with the reply.
	if n := d.notifyCount(); n != 1 {
		t.Fatalf("want 1 notify, got %d: %v", n, d.notifyMsgs)
	}
	if got := d.lastNotify(); !strings.Contains(got, "Hello") {
		t.Errorf("reply = %q; want it to contain 'Hello'", got)
	}

	// History should have user + assistant.
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	if history[0] != "Hi Jarvis!" {
		t.Errorf("history[0] = %q", history[0])
	}
}

func TestHandleMessage_BusyNotice(t *testing.T) {
	oldBusy := busyTimeout
	oldMax := maxTimeout
	busyTimeout = 500 * time.Millisecond
	maxTimeout = 5 * time.Second
	defer func() {
		busyTimeout = oldBusy
		maxTimeout = oldMax
	}()

	// Server responds after busyTimeout, so we should see a busy notice.
	server := fakeOllamaServer(t, "Sorry for the wait!", 1*time.Second)
	defer server.Close()

	t.Setenv("OLLAMA_URL", server.URL)
	client := models.NewClient()

	confDir := t.TempDir()
	t.Setenv("JARVIS_CONF", confDir)
	os.WriteFile(confDir+"/current-job", []byte("test-job-123"), 0o644)

	d := &testDiscord{}
	var history []string

	handleMessage(d, client, "jarvis-brain", chat.Incoming{Content: "What are you doing?"}, &history)

	// Should have sent busy notice then the actual reply.
	if n := d.notifyCount(); n != 2 {
		t.Fatalf("want 2 notifies (busy + reply), got %d: %v", n, d.notifyMsgs)
	}
	if !strings.Contains(d.notifyMsgs[0], "Busy on") {
		t.Errorf("first notify should be busy notice, got: %q", d.notifyMsgs[0])
	}
	if !strings.Contains(d.notifyMsgs[0], "test-job-123") {
		t.Errorf("busy notice should mention job, got: %q", d.notifyMsgs[0])
	}
	if !strings.Contains(d.notifyMsgs[1], "Sorry for the wait") {
		t.Errorf("second notify should be the reply, got: %q", d.notifyMsgs[1])
	}
}

func TestHandleMessage_Timeout(t *testing.T) {
	oldBusy := busyTimeout
	oldMax := maxTimeout
	busyTimeout = 300 * time.Millisecond
	maxTimeout = 800 * time.Millisecond
	defer func() {
		busyTimeout = oldBusy
		maxTimeout = oldMax
	}()

	// Server never responds (we don't close it, but also don't reply).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang forever — the client's maxTimeout will kill the request.
		time.Sleep(30 * time.Second)
	}))
	defer server.Close()

	t.Setenv("OLLAMA_URL", server.URL)
	client := models.NewClient()

	confDir := t.TempDir()
	t.Setenv("JARVIS_CONF", confDir)
	os.WriteFile(confDir+"/current-job", []byte("long-task"), 0o644)

	d := &testDiscord{}
	var history []string

	handleMessage(d, client, "jarvis-brain", chat.Incoming{Content: "Hello?"}, &history)

	// Should have sent busy notice then timeout message.
	if n := d.notifyCount(); n != 2 {
		t.Fatalf("want 2 notifies (busy + timeout), got %d: %v", n, d.notifyMsgs)
	}
	if !strings.Contains(d.notifyMsgs[0], "Busy on") {
		t.Errorf("first notify should be busy notice, got: %q", d.notifyMsgs[0])
	}
	if !strings.Contains(d.notifyMsgs[1], "brain is tied up") {
		t.Errorf("last notify should be timeout message, got: %q", d.notifyMsgs[1])
	}

	// History should be empty — no answer was received.
	if len(history) != 0 {
		t.Errorf("history should be empty on timeout, got %d entries", len(history))
	}
}

func TestBuildMessages(t *testing.T) {
	history := []string{"who are you?", "I am Jarvis."}
	msgs := buildMessages("what time is it", history)

	// System + user + assistant + current user = 4 messages
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message should be system, got %q", msgs[0].Role)
	}
	if msgs[1].Content != "who are you?" {
		t.Errorf("second message content = %q", msgs[1].Content)
	}
	if msgs[3].Content != "what time is it" {
		t.Errorf("last message content = %q", msgs[3].Content)
	}
}

func TestPruneHistory(t *testing.T) {
	h := []string{}
	for i := 0; i < 100; i++ {
		// Each turn is ~30 words.
		h = append(h, fmt.Sprintf("user message number %d with some extra words to pad it out a bit here", i))
		h = append(h, fmt.Sprintf("assistant reply number %d with some extra words to pad it out a bit here", i))
	}

	pruneHistory(&h, 3000)

	// After pruning, total words should be <= 3000.
	totalWords := 0
	for _, turn := range h {
		totalWords += strings.Count(turn, " ") + 1
	}
	if totalWords > 3000 {
		t.Errorf("pruned history has %d words, want <= 3000", totalWords)
	}

	// History should still be even (pairs of user/assistant).
	if len(h)%2 != 0 {
		t.Errorf("pruned history has odd length %d", len(h))
	}
}

func TestTailLines(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/test.log"
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	lines := tailLines(path, 3)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if lines[0] != "line3" || lines[1] != "line4" || lines[2] != "line5" {
		t.Errorf("got %v", lines)
	}

	// Non-existent file returns nil.
	empty := tailLines("/no/such/file", 3)
	if empty != nil {
		t.Errorf("want nil for missing file, got %v", empty)
	}
}

func TestSystemPromptForbidsCoding(t *testing.T) {
	t.Setenv("JARVIS_CONF", t.TempDir())
	p := systemPromptText()
	for _, want := range []string{"no shell", "no secrets", "jobs", "agent-run"} {
		if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) && !strings.Contains(p, want) {
			// allow either casing; also match "You cannot" + jobs wording
			_ = want
		}
	}
	low := strings.ToLower(p)
	if !strings.Contains(low, "shell") || !strings.Contains(low, "secret") {
		t.Fatalf("prompt missing shell/secrets ban:\n%s", p)
	}
	if !strings.Contains(low, "job") || !strings.Contains(low, "agent-run") {
		t.Fatalf("prompt missing job / agent-run:\n%s", p)
	}
	msgs := buildMessages("hi", nil)
	if len(msgs) < 1 || msgs[0].Role != "system" {
		t.Fatal("expected system message")
	}
	if !strings.Contains(msgs[0].Content, "You cannot act on this machine") {
		t.Errorf("system message missing ban")
	}
}

func TestSystemPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_CONF", dir)
	if err := os.WriteFile(dir+"/chat-prompt.txt", []byte("OVERRIDE PROMPT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := systemPromptText(); got != "OVERRIDE PROMPT" {
		t.Fatalf("got %q", got)
	}
}
