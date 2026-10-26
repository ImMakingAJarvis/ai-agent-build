package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReplyToEngineSendsIDSlot is the acceptance test for s5.1: the reply
// request must carry the id_slot field pinning it to the chat slot.
func TestReplyToEngineSendsIDSlot(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi there"}}]}`)
	}))
	defer srv.Close()

	msgs := buildMessages("s5.1", "Chat goes to the chat slot", "building", "hello")
	out, err := replyToEngine(srv.URL, "jarvis-brain", 1, msgs, 5*time.Second)
	if err != nil {
		t.Fatalf("replyToEngine: %v", err)
	}
	if out != "hi there" {
		t.Errorf("reply = %q, want %q", out, "hi there")
	}
	if got["model"] != "jarvis-brain" {
		t.Errorf("model = %v, want jarvis-brain", got["model"])
	}
	if got["id_slot"] != float64(1) {
		t.Errorf("id_slot = %v, want 1", got["id_slot"])
	}
	if got["max_tokens"] != float64(replyMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got["max_tokens"], replyMaxTokens)
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false", got["stream"])
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), "Nathan: hello") {
		t.Errorf("request messages missing Nathan's message: %s", raw)
	}
}

// TestReplyToEngineTimesOut checks the 2-minute-limit path: a slow engine
// yields context.DeadlineExceeded, which main turns into the busy message.
func TestReplyToEngineTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"too late"}}]}`)
	}))
	defer srv.Close()

	_, err := replyToEngine(srv.URL, "jarvis-brain", 1,
		[]chatMessage{{Role: "user", Content: "hello"}}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestReplyToEngineEngineError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := replyToEngine(srv.URL, "jarvis-brain", 1,
		[]chatMessage{{Role: "user", Content: "hello"}}, 5*time.Second)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, must not look like a timeout", err)
	}
}

func TestBuildMessages(t *testing.T) {
	msgs := buildMessages("s5.1", "Chat slot", "line-a\nline-b", "what's up?")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message role = %q, want system", msgs[0].Role)
	}
	u := msgs[1]
	if u.Role != "user" {
		t.Errorf("second message role = %q, want user", u.Role)
	}
	for _, want := range []string{"s5.1", "Chat slot", "line-b", "what's up?"} {
		if !contains(u.Content, want) {
			t.Errorf("user message missing %q:\n%s", want, u.Content)
		}
	}
}

func TestBusyMessage(t *testing.T) {
	if got := busyMessage("s5.1"); got != "I'm busy on s5.1, I'll answer after it" {
		t.Errorf("busyMessage = %q", got)
	}
	if got := busyMessage(""); got == "" {
		t.Error("busyMessage(\"\") must not be empty")
	}
}

func TestSlotFlag(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("CHAT_SLOT_ID", "")
		if got := slotFlag(""); got != defaultSlot {
			t.Errorf("slotFlag = %d, want %d", got, defaultSlot)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("CHAT_SLOT_ID", "3")
		if got := slotFlag(""); got != 3 {
			t.Errorf("slotFlag = %d, want 3", got)
		}
	})
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("CHAT_SLOT_ID", "3")
		if got := slotFlag("2"); got != 2 {
			t.Errorf("slotFlag = %d, want 2", got)
		}
	})
}

func TestReadLocalEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local.env"),
		[]byte("# comment\nENGINE_URL=http://localhost:9999\nLOCAL_MODEL=brain\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := readLocalEnv(dir)
	if env["ENGINE_URL"] != "http://localhost:9999" {
		t.Errorf("ENGINE_URL = %q", env["ENGINE_URL"])
	}
	if env["LOCAL_MODEL"] != "brain" {
		t.Errorf("LOCAL_MODEL = %q", env["LOCAL_MODEL"])
	}

	if got := envOr("ENGINE_URL", "def", env); got != "http://localhost:9999" {
		t.Errorf("envOr = %q", got)
	}
	if got := envOr("MISSING_KEY", "def", env); got != "def" {
		t.Errorf("envOr = %q", got)
	}
}

func TestReadLocalEnvMissingFile(t *testing.T) {
	env := readLocalEnv(t.TempDir())
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
	if got := envOr("ENGINE_URL", "def", env); got != "def" {
		t.Errorf("envOr = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}
