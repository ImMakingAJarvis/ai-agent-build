package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/voices"
)

func TestParseSchedulePrefix(t *testing.T) {
	raw := "I'll queue that.\n\nSCHEDULE:\ntitle: add /health\ninstructions:\nAdd a tiny /health handler.\nKeep it small.\n"
	b, found, bad, stripped := parseSchedule(raw)
	if !found || bad {
		t.Fatalf("found=%v malformed=%v", found, bad)
	}
	if b.Title != "add /health" {
		t.Fatalf("title %q", b.Title)
	}
	if !strings.Contains(b.Instructions, "/health") {
		t.Fatalf("instructions %q", b.Instructions)
	}
	if strings.Contains(stripped, "SCHEDULE") {
		t.Fatalf("not stripped: %q", stripped)
	}
}

func TestParseScheduleFence(t *testing.T) {
	raw := "ok\n```schedule\ntitle: fix fan\ninstructions: check OpenRGB\n```\n"
	b, found, bad, _ := parseSchedule(raw)
	if !found || bad || b.Title != "fix fan" {
		t.Fatalf("%+v found=%v bad=%v", b, found, bad)
	}
}

func TestParseScheduleMalformed(t *testing.T) {
	_, found, bad, _ := parseSchedule("SCHEDULE:\ntitle: only\n")
	if !found || !bad {
		t.Fatalf("want malformed, found=%v bad=%v", found, bad)
	}
}

func TestParseScheduleNone(t *testing.T) {
	_, found, bad, stripped := parseSchedule("just chatting")
	if found || bad || stripped != "just chatting" {
		t.Fatalf("found=%v bad=%v stripped=%q", found, bad, stripped)
	}
}

func TestEnqueueFromChatAndConfirm(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	t.Setenv("JARVIS_CONF", dir)
	in := chat.Incoming{ChannelID: "c1", MessageID: "m1", Content: "!build add /health\nsmall job"}
	title, body := titleAndBody("add /health\nsmall job")
	id, err := enqueueFromChat(in, agentjob.ClassBuild, title, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "chat-") {
		t.Fatalf("id %q", id)
	}
	s, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Source != "discord" || j.Driver != "local" || j.DiscordMessageID != "m1" {
		t.Fatalf("%+v", j.Spec)
	}
	if j.Class != agentjob.ClassBuild {
		t.Fatalf("!build job class = %q, want build", j.Class)
	}
	if _, err := os.Stat(filepath.Join(dir, "pause-signal")); !os.IsNotExist(err) {
		t.Fatal("pause-signal must not be created")
	}
	confirm := enqueueConfirm(id)
	if !strings.Contains(confirm, id) {
		t.Fatalf("confirm %q", confirm)
	}
}

func TestEnqueueFromChatFixClass(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	in := chat.Incoming{ChannelID: "c1", MessageID: "m1"}
	id, err := enqueueFromChat(in, agentjob.ClassFix, "fix the fan curve", "body")
	if err != nil {
		t.Fatal(err)
	}
	s, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Class != agentjob.ClassFix {
		t.Fatalf("!fix job class = %q, want fix", j.Class)
	}
}

func TestApplyScheduleGoodAndMalformed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	in := chat.Incoming{ChannelID: "c", MessageID: "m"}
	vis, extra := applySchedule("Sure.\nSCHEDULE:\ntitle: t\ninstructions: do it\n", in)
	if strings.Contains(vis, "SCHEDULE") {
		t.Fatalf("visible still has block: %q", vis)
	}
	if !strings.Contains(extra, "Queued agent job") {
		t.Fatalf("extra %q", extra)
	}
	vis, extra = applySchedule("SCHEDULE:\nnope\n", in)
	if !strings.Contains(extra, "malformed") {
		t.Fatalf("extra %q", extra)
	}
	if vis == "" && extra == "" {
		t.Fatal("empty")
	}
	vis, extra = applySchedule("hello", in)
	if extra != "" || vis != "hello" {
		t.Fatalf("normal chat: %q %q", vis, extra)
	}
}

func TestCutBuildFix(t *testing.T) {
	kind, rest, ok := cutBuildFix("!build add a handler")
	if !ok || kind != "build" || rest != "add a handler" {
		t.Fatalf("%q %q %v", kind, rest, ok)
	}
	kind, rest, ok = cutBuildFix("!FIX the fan curve")
	if !ok || kind != "fix" || rest != "the fan curve" {
		t.Fatalf("%q %q %v", kind, rest, ok)
	}
	if _, _, ok := cutBuildFix("hello"); ok {
		t.Fatal("want miss")
	}
}

func TestHandleMessageScheduleEnqueues(t *testing.T) {
	oldBusy := busyTimeout
	oldMax := maxTimeout
	busyTimeout = 500 * time.Millisecond
	maxTimeout = 2 * time.Second
	defer func() {
		busyTimeout = oldBusy
		maxTimeout = oldMax
	}()

	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	t.Setenv("JARVIS_CONF", dir)

	// "add a health endpoint" is fast-classified as chat, so the
	// classify LLM call is skipped. The server only receives the
	// Chat call, which should return the SCHEDULE response.
	scheduleReply := "On it.\nSCHEDULE:\ntitle: tiny health\ninstructions: add /health\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string             `json:"model"`
			Messages []models.ChatMessage `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		time.Sleep(50 * time.Millisecond)

		// Return the SCHEDULE response.
		resp := models.ChatResponse{
			Choices: []struct {
				Message models.ChatMessage `json:"message"`
			}{
				{Message: models.ChatMessage{Role: "assistant", Content: scheduleReply}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	t.Setenv("OLLAMA_URL", server.URL)
	client := models.NewClient()
	hAgent := newTestHermesAgent(client)

	d := &testDiscord{}
	var history []string
	handleMessage(d, hAgent, voices.Get(""), chat.Incoming{
		ChannelID: "ch", MessageID: "msg", Content: "add a health endpoint",
	}, &history)

	if n := d.notifyCount(); n != 1 {
		t.Fatalf("notifies %d %v", n, d.notifyMsgs)
	}
	got := d.lastNotify()
	if strings.Contains(got, "SCHEDULE:") {
		t.Fatalf("reply still has SCHEDULE: %q", got)
	}
	if !strings.Contains(got, "Queued agent job") {
		t.Fatalf("reply %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pause-signal")); !os.IsNotExist(err) {
		t.Fatal("pause-signal after SCHEDULE")
	}
	s, _ := agentjob.Open(dir)
	jobs, _ := s.List()
	if len(jobs) != 1 || jobs[0].Title != "tiny health" || jobs[0].DiscordMessageID != "msg" {
		t.Fatalf("jobs %+v", jobs)
	}
}

func TestPauseStillWritesSignal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_CONF", dir)
	if err := os.WriteFile(dir+"/pause-signal", []byte("!pause"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pauseSignalPath()); err != nil {
		t.Fatal(err)
	}
}