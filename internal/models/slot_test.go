package models

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestChatSlotSendsIDSlot is the acceptance test for s5.1: the chat-slot
// request must carry id_slot pinning it to the llama.cpp parallel slot.
func TestChatSlotSendsIDSlot(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"slot reply"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_URL", srv.URL)
	c := NewClient()
	out, err := c.ChatSlot("jarvis-brain",
		[]ChatMessage{{Role: "user", Content: "hello"}}, 1)
	if err != nil {
		t.Fatalf("ChatSlot: %v", err)
	}
	if out != "slot reply" {
		t.Errorf("reply = %q, want %q", out, "slot reply")
	}
	if got["id_slot"] != float64(1) {
		t.Errorf("id_slot = %v, want 1", got["id_slot"])
	}
	if got["model"] != "jarvis-brain" {
		t.Errorf("model = %v, want jarvis-brain", got["model"])
	}
	if got["max_tokens"] != float64(chatSlotMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got["max_tokens"], chatSlotMaxTokens)
	}
}

// TestChatSlotPassesThroughSlotID checks a non-default slot value.
func TestChatSlotPassesThroughSlotID(t *testing.T) {
	var gotIDSlot any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotIDSlot = body["id_slot"]
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_URL", srv.URL)
	c := NewClient()
	if _, err := c.ChatSlot("m", []ChatMessage{{Role: "user", Content: "hi"}}, 7); err != nil {
		t.Fatalf("ChatSlot: %v", err)
	}
	if gotIDSlot != float64(7) {
		t.Errorf("id_slot = %v, want 7", gotIDSlot)
	}
}

func TestChatSlotEngineError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slot busy", http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_URL", srv.URL)
	c := NewClient()
	if _, err := c.ChatSlot("m", []ChatMessage{{Role: "user", Content: "hi"}}, 1); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestChatSlotNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_URL", srv.URL)
	c := NewClient()
	if _, err := c.ChatSlot("m", []ChatMessage{{Role: "user", Content: "hi"}}, 1); err == nil {
		t.Fatal("expected error on empty choices")
	}
}
