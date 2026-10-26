package hermes

import (
	"context"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// slotFake records which path the agent used: slot-pinned ChatSlot or plain
// Chat. It satisfies SlotChatClient.
type slotFake struct {
	chatCalls int
	slotCalls int
	lastSlot  int
	reply     string
}

func (f *slotFake) Chat(string, []models.ChatMessage) (string, error) {
	f.chatCalls++
	return f.reply, nil
}

func (f *slotFake) ChatSlot(model string, messages []models.ChatMessage, idSlot int) (string, error) {
	f.slotCalls++
	f.lastSlot = idSlot
	return f.reply, nil
}

// plainFake only implements ChatClient, like the older test fakes.
type plainFake struct{ calls int }

func (f *plainFake) Chat(string, []models.ChatMessage) (string, error) {
	f.calls++
	return "plain", nil
}

func TestChatUsesSlotWhenConfigured(t *testing.T) {
	fake := &slotFake{reply: "hi"}
	a := New(Config{Model: "m", ChatSlot: 1})
	a.SetOllamaClient(fake)

	out, err := a.Chat(context.Background(), "sys", []string{"u", "a"}, "hello")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "hi" {
		t.Errorf("reply = %q, want %q", out, "hi")
	}
	if fake.slotCalls != 1 {
		t.Errorf("slotCalls = %d, want 1", fake.slotCalls)
	}
	if fake.chatCalls != 0 {
		t.Errorf("chatCalls = %d, want 0", fake.chatCalls)
	}
	if fake.lastSlot != 1 {
		t.Errorf("lastSlot = %d, want 1", fake.lastSlot)
	}
}

func TestClassifyUsesSlotWhenConfigured(t *testing.T) {
	fake := &slotFake{reply: `{"type":"chat"}`}
	a := New(Config{Model: "m", ChatSlot: 1})
	a.SetOllamaClient(fake)

	// A 30+ word message: the fast path leaves long messages to the LLM.
	longMsg := "I was wondering if you could maybe look into whether the engine could be made a bit faster for the build rounds sometime this week if you have the cycles to spare for that"
	if _, err := a.Classify(context.Background(), longMsg); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if fake.slotCalls != 1 || fake.chatCalls != 0 {
		t.Errorf("slotCalls=%d chatCalls=%d, want 1/0", fake.slotCalls, fake.chatCalls)
	}
}

func TestChatFallsBackWhenClientHasNoSlots(t *testing.T) {
	fake := &plainFake{}
	a := New(Config{Model: "m", ChatSlot: 1})
	a.SetOllamaClient(fake)

	out, err := a.Chat(context.Background(), "sys", nil, "hello")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out != "plain" {
		t.Errorf("reply = %q, want %q", out, "plain")
	}
	if fake.calls != 1 {
		t.Errorf("plain Chat calls = %d, want 1", fake.calls)
	}
}

func TestChatUsesPlainChatWhenSlotZero(t *testing.T) {
	fake := &slotFake{reply: "hi"}
	a := New(Config{Model: "m"}) // ChatSlot unset = 0 = any slot
	a.SetOllamaClient(fake)

	if _, err := a.Chat(context.Background(), "sys", nil, "hello"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if fake.slotCalls != 0 || fake.chatCalls != 1 {
		t.Errorf("slotCalls=%d chatCalls=%d, want 0/1", fake.slotCalls, fake.chatCalls)
	}
}
