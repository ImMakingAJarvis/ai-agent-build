// Test the jarvis-chat daemon's handleMessage flow.
//
// We spin up a fake Ollama server and use a test-harness Discord type
// to verify: normal reply, busy notice, and timeout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/hermes"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/memory"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photomind"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/voices"
)

// deadMemory points at a closed port: Search fails open and returns nil,
// keeping the handleMessage tests hermetic (no live memory service).
var deadMemory = memory.NewClient("http://127.0.0.1:1")

// testDiscord mimics *chat.Discord for tests.
// It records Notify and Typing calls so we can assert on them.
type testDiscord struct {
	mu          sync.Mutex
	notifyMsgs  []string
	typingCalls int
	typingErr   error
	notifyErr   error
	photoMsgs   []photoSend
	sendImageErr error
}

// photoSend is one SendImage call recorded by testDiscord.
type photoSend struct {
	text string
	path string
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

func (d *testDiscord) SendImage(_ context.Context, text, path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sendImageErr != nil {
		return d.sendImageErr
	}
	d.photoMsgs = append(d.photoMsgs, photoSend{text, path})
	return nil
}

func (d *testDiscord) lastPhoto() (photoSend, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.photoMsgs) == 0 {
		return photoSend{}, false
	}
	return d.photoMsgs[len(d.photoMsgs)-1], true
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
//
// The server handles two phases: the first call is a Hermes classify
// request (returns a JSON "chat" result), and subsequent calls are
// Hermes Chat requests (return the reply text).
func fakeOllamaServer(t *testing.T, reply string, delay time.Duration) *httptest.Server {
	t.Helper()
	var callCount int
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		var req struct {
			Model    string           `json:"model"`
			Messages []models.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		mu.Lock()
		callCount++
		isFirst := callCount == 1
		mu.Unlock()

		// First call: Hermes classify — return JSON chat result.
		if isFirst {
			classifyReply := fmt.Sprintf(`{"type":"chat","reply":"%s"}`, reply)
			resp := models.ChatResponse{
				Choices: []struct {
					Message models.ChatMessage `json:"message"`
				}{
					{Message: models.ChatMessage{Role: "assistant", Content: classifyReply}},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Subsequent calls: Hermes Chat — return the reply.
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

// newTestHermesAgent creates a Hermes agent wired to the Ollama client
// for use in tests. Default actions are registered so fast-classified
// action messages (like "check status") have handlers.
func newTestHermesAgent(client *models.Client) *hermes.Agent {
	agent := hermes.New(hermes.Config{
		Model:       "jarvis-brain",
		MaxHistory:  10,
		AccessLevel: config.AccessFull,
	})
	agent.SetOllamaClient(client)
	for _, a := range hermes.DefaultActions() {
		agent.Register(a)
	}
	return agent
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

	t.Setenv("OLLAMA_URL", server.URL)
	logDir := t.TempDir()
	t.Setenv("JARVIS_NOTES_DIR", logDir)
	client := models.NewClient()
	hAgent := newTestHermesAgent(client)

	d := &testDiscord{}
	var history []string

	handleMessage(d, hAgent, voices.Get(""), deadMemory, chat.Incoming{Content: "Hi Jarvis!"}, &history)

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

	// The exchange should be recorded in the conversation log.
	logName := filepath.Join(logDir, "discord-"+time.Now().Format("2006-01-02")+".md")
	b, err := os.ReadFile(logName)
	if err != nil {
		t.Fatalf("conversation log: %v", err)
	}
	if s := string(b); !strings.Contains(s, "Nathan: Hi Jarvis!") || !strings.Contains(s, "Hello") {
		t.Errorf("conversation log = %q; want it to contain the exchange", s)
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
	t.Setenv("JARVIS_NOTES_DIR", t.TempDir())
	client := models.NewClient()
	hAgent := newTestHermesAgent(client)

	confDir := t.TempDir()
	t.Setenv("JARVIS_CONF", confDir)
	os.WriteFile(confDir+"/current-job", []byte("test-job-123"), 0o644)

	d := &testDiscord{}
	var history []string

	// "What are you working on?" is fast-classified as check_status action,
	// so use a message that's clearly conversational to exercise the chat path.
	handleMessage(d, hAgent, voices.Get(""), deadMemory, chat.Incoming{Content: "Tell me a fun fact about robots"}, &history)

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
	t.Setenv("JARVIS_NOTES_DIR", t.TempDir())
	client := models.NewClient()
	hAgent := newTestHermesAgent(client)

	confDir := t.TempDir()
	t.Setenv("JARVIS_CONF", confDir)
	os.WriteFile(confDir+"/current-job", []byte("long-task"), 0o644)

	d := &testDiscord{}
	var history []string

	handleMessage(d, hAgent, voices.Get(""), deadMemory, chat.Incoming{Content: "Hello?"}, &history)

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
	// The system prompt comes from the voice registry.
	voice := voices.Get("")
	p := voice.SystemPrompt
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

func TestSystemPromptVoiceSelection(t *testing.T) {
	// Verify that different voices return different system prompts.
	jarvis := voices.Get("jarvis")
	if jarvis == nil {
		t.Fatal("jarvis voice not found")
	}
	if !strings.Contains(jarvis.SystemPrompt, "Jarvis") {
		t.Errorf("jarvis voice missing 'Jarvis' in prompt")
	}

	glados := voices.Get("glados")
	if glados == nil {
		t.Fatal("glados voice not found")
	}
	if !strings.Contains(glados.SystemPrompt, "GLaDOS") {
		t.Errorf("glados voice missing 'GLaDOS' in prompt")
	}

	// Unknown key falls back to default.
	unknown := voices.Get("nonexistent")
	if unknown == nil {
		t.Fatal("unknown voice returned nil")
	}
	if unknown.Key != jarvis.Key {
		t.Errorf("fallback voice key = %q, want %q", unknown.Key, jarvis.Key)
	}
}

func TestLogConversation_WritesExchange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_NOTES_DIR", dir)

	logConversation("hello there", "Hi! How can I help?")
	logConversation("second", "ok")

	name := filepath.Join(dir, "discord-"+time.Now().Format("2006-01-02")+".md")
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("log file: %v", err)
	}
	for _, want := range []string{
		"Nathan: hello there",
		"Jarvis:",
		"Hi! How can I help?",
		"Nathan: second",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log missing %q in:\n%s", want, b)
		}
	}
}

func TestLogConversation_Disabled(t *testing.T) {
	// Explicitly empty dir disables logging; must not panic or write.
	t.Setenv("JARVIS_NOTES_DIR", "")
	logConversation("a", "b")
}

// TestHandleMessage_MemoryContext verifies that chat replies get relevant
// work-log context from the memory service injected into the system
// prompt (RAG), and that a dead service leaves the prompt untouched.
func TestHandleMessage_MemoryContext(t *testing.T) {
	oldBusy := busyTimeout
	oldMax := maxTimeout
	busyTimeout = 500 * time.Millisecond
	maxTimeout = 2 * time.Second
	defer func() {
		busyTimeout = oldBusy
		maxTimeout = oldMax
	}()

	// Fake memory service: one hit about the wake word.
	memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"text":"wake word latency measured at 1.2s","score":0.7,"source":"job","path":"job-1/transcript.txt","title":"wake-latency"}]}`)
	}))
	defer memSrv.Close()

	// Fake Ollama that records the Chat request bodies.
	var mu sync.Mutex
	var bodies []string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []models.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, req.Messages[0].Content) // system prompt of each call
		mu.Unlock()
		reply := `{"type":"chat","reply":"it was 1.2s"}`
		if len(bodies) > 1 {
			reply = "it was 1.2s"
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
	defer ollama.Close()

	t.Setenv("OLLAMA_URL", ollama.URL)
	t.Setenv("JARVIS_NOTES_DIR", t.TempDir())
	client := models.NewClient()
	hAgent := newTestHermesAgent(client)
	d := &testDiscord{}
	var history []string

	mem := memory.NewClient(memSrv.URL)
	handleMessage(d, hAgent, voices.Get(""), mem,
		chat.Incoming{Content: "what was the wake word latency again?"}, &history)

	mu.Lock()
	if len(bodies) < 1 {
		t.Fatalf("want a chat call, got %d", len(bodies))
	}
	chatSystem := bodies[len(bodies)-1] // last call is the chat reply
	for _, want := range []string{
		"Relevant context from the work logs",
		"job", "job-1/transcript.txt", "1.2s",
	} {
		if !strings.Contains(chatSystem, want) {
			t.Errorf("chat system prompt missing %q:\n%s", want, chatSystem)
		}
	}

	// Dead memory service: no context block, reply still works.
	bodies = nil
	mu.Unlock()
	handleMessage(d, hAgent, voices.Get(""), deadMemory,
		chat.Incoming{Content: "hello?"}, &history)
	mu.Lock()
	if len(bodies) < 1 {
		t.Fatalf("want a chat call, got %d", len(bodies))
	}
	if strings.Contains(bodies[len(bodies)-1], "Relevant context from the work logs") {
		t.Errorf("dead memory service must not add context:\n%s", bodies[len(bodies)-1])
	}
}

// fakeArtServer emulates the art service's /generate endpoint. When
// prompts is non-nil it records the prompt of every request, in order.
func fakeArtServer(t *testing.T, status int, body string, prompts *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if prompts != nil {
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Prompt string `json:"prompt"`
			}
			_ = json.Unmarshal(b, &req)
			mu.Lock()
			*prompts = append(*prompts, req.Prompt)
			mu.Unlock()
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

// fakePhotoBrain is a scripted thinking mind for the photo tests.
type fakePhotoBrain struct {
	replies []string
	err     error
	healthy bool
	calls   int
}

func (f *fakePhotoBrain) Chat(_ string, _ []models.ChatMessage) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	i := f.calls
	f.calls++
	if i >= len(f.replies) {
		return "", errors.New("fakePhotoBrain: out of scripted replies")
	}
	return f.replies[i], nil
}

func (f *fakePhotoBrain) Healthcheck() bool { return f.healthy }

// photoTestService wires the fake brain to the fake art service.
func photoTestService(t *testing.T, brain *fakePhotoBrain, artURL string) *photomind.Service {
	t.Helper()
	return photomind.New(brain, "test-model", art.NewWithURL(artURL))
}

func TestCutPhotoCmd(t *testing.T) {
	cases := []struct {
		in   string
		mode string
		want string
		ok   bool
	}{
		{"!photo a red panda in snow", "", "a red panda in snow", true},
		{"!photo    spaced out  ", "", "spaced out", true},
		{"!photo", "", "", true},
		{"!photoassist a red panda", "assist", "a red panda", true},
		{"!photoassist", "assist", "", true},
		{"!photocheck a red panda", "check", "a red panda", true},
		{"!photocheck", "check", "", true},
		{"!photograph a sunset", "", "", false},
		{"!photochecker a sunset", "", "", false},
		{"hello !photo x", "", "", false},
		{"!build something", "", "", false},
	}
	for _, c := range cases {
		mode, got, ok := cutPhotoCmd(c.in)
		if ok != c.ok || (ok && (got != c.want || mode != c.mode)) {
			t.Errorf("cutPhotoCmd(%q) = %q, %q, %v; want mode %q, prompt %q, ok %v",
				c.in, mode, got, ok, c.mode, c.want, c.ok)
		}
	}
}

func TestHandlePhotoSendsImage(t *testing.T) {
	// A throwaway PNG where the fake art service says the image landed.
	png := filepath.Join(t.TempDir(), "20261028-000000-42.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	expanded := "A photo of a red panda sitting in fresh snow at dawn, soft blue light."
	var prompts []string
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":42,"steps":9,"seconds":25.3}`, png), &prompts)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{expanded}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "", "a red panda in fresh snow")

	ph, ok := d.lastPhoto()
	if !ok {
		t.Fatalf("no image sent; notifies: %v", d.notifyMsgs)
	}
	if ph.path != png {
		t.Errorf("sent path %q, want %q", ph.path, png)
	}
	// The art service must receive the thinking mind's expanded prompt,
	// not the owner's raw words.
	if len(prompts) != 1 || prompts[0] != expanded {
		t.Errorf("art service got %v, want the expanded prompt", prompts)
	}
	for _, want := range []string{expanded, "seed 42", "1024x1024", "25s"} {
		if !strings.Contains(ph.text, want) {
			t.Errorf("caption missing %q:\n%s", want, ph.text)
		}
	}
	if d.typingCount() == 0 {
		t.Error("no typing indicator during generation")
	}
}

func TestHandlePhotoBrainDown(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":7,"steps":9,"seconds":10}`, png), &prompts)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{"MUST NOT BE USED"}, healthy: false}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "", "a red panda")

	ph, ok := d.lastPhoto()
	if !ok {
		t.Fatalf("no image sent; notifies: %v", d.notifyMsgs)
	}
	if len(prompts) != 1 || prompts[0] != "a red panda" {
		t.Errorf("art service got %v, want the raw prompt", prompts)
	}
	if !strings.Contains(ph.text, "thinking mind is down") {
		t.Errorf("caption missing the fallback note:\n%s", ph.text)
	}
	if brain.calls != 0 {
		t.Error("the brain should not have been called while down")
	}
}

func TestHandlePhotoAssistSendsPromptMessage(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	expanded := "A photo of a red panda in fresh snow, dawn light, 85mm lens."
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":42,"steps":9,"seconds":1}`, png), nil)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{expanded}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "assist", "red panda in snow")

	// The full prompt goes out as a chat message before the image, so
	// the owner can read and reuse it.
	if got := d.lastNotify(); !strings.Contains(got, expanded) {
		t.Errorf("prompt message = %q", got)
	}
	ph, ok := d.lastPhoto()
	if !ok {
		t.Fatalf("no image sent; notifies: %v", d.notifyMsgs)
	}
	if !strings.Contains(ph.text, expanded) {
		t.Errorf("caption missing the prompt:\n%s", ph.text)
	}
}

func TestHandlePhotoCheckIterates(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	p1 := "A photo of a red panda."
	p2 := "A photo of a red panda in fresh snow at dawn, close-up, 85mm."
	var prompts []string
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":42,"steps":9,"seconds":1}`, png), &prompts)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{p1, p2}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "check", "red panda in snow")

	ph, ok := d.lastPhoto()
	if !ok {
		t.Fatalf("no image sent; notifies: %v", d.notifyMsgs)
	}
	if len(prompts) != 2 || prompts[0] != p1 || prompts[1] != p2 {
		t.Errorf("art service got %v, want the draft then the improved prompt", prompts)
	}
	for _, want := range []string{"2 passes", p2} {
		if !strings.Contains(ph.text, want) {
			t.Errorf("caption missing %q:\n%s", want, ph.text)
		}
	}
}

func TestHandlePhotoCheckSinglePassWhenUnchanged(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	p := "A photo of a red panda in fresh snow at dawn."
	var prompts []string
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":42,"steps":9,"seconds":1}`, png), &prompts)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{p, p}, healthy: true} // critique finds nothing to fix
	handlePhoto(d, photoTestService(t, brain, srv.URL), "check", "red panda in snow")

	if len(prompts) != 1 {
		t.Errorf("expected 1 generation, got %d: %v", len(prompts), prompts)
	}
	ph, ok := d.lastPhoto()
	if !ok {
		t.Fatalf("no image sent; notifies: %v", d.notifyMsgs)
	}
	if strings.Contains(ph.text, "2 passes") {
		t.Errorf("caption should not claim 2 passes:\n%s", ph.text)
	}
}

func TestHandlePhotoBusy(t *testing.T) {
	srv := fakeArtServer(t, http.StatusConflict, `{"detail":"a generation is already running"}`, nil)
	defer srv.Close()

	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{"expanded"}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "", "a red panda")
	if got := d.lastNotify(); !strings.Contains(got, "already generating") {
		t.Errorf("busy reply = %q", got)
	}
	if _, ok := d.lastPhoto(); ok {
		t.Error("no image should be sent while busy")
	}
}

func TestHandlePhotoUnavailable(t *testing.T) {
	d := &testDiscord{}
	brain := &fakePhotoBrain{replies: []string{"expanded"}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, "http://127.0.0.1:1"), "", "a red panda")
	if got := d.lastNotify(); got == "" || strings.Contains(got, "seed") {
		t.Errorf("unavailable reply = %q", got)
	}
}

func TestHandlePhotoEmptyPrompt(t *testing.T) {
	d := &testDiscord{}
	brain := &fakePhotoBrain{healthy: true}
	handlePhoto(d, photoTestService(t, brain, "http://127.0.0.1:1"), "assist", "   ")
	if got := d.lastNotify(); !strings.Contains(got, "Need a prompt after !photoassist") {
		t.Errorf("empty prompt reply = %q", got)
	}
}

func TestHandlePhotoSendFails(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(png, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := fakeArtServer(t, http.StatusOK,
		fmt.Sprintf(`{"path":%q,"width":1024,"height":1024,"seed":42,"steps":9,"seconds":1}`, png), nil)
	defer srv.Close()

	d := &testDiscord{sendImageErr: errors.New("discord down")}
	brain := &fakePhotoBrain{replies: []string{"expanded"}, healthy: true}
	handlePhoto(d, photoTestService(t, brain, srv.URL), "", "a red panda")
	if got := d.lastNotify(); !strings.Contains(got, "could not send") {
		t.Errorf("send failure reply = %q", got)
	}
}
