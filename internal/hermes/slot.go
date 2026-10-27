package hermes

import (
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// SlotChatClient is implemented by chat clients that can pin a request to
// a llama.cpp parallel slot. *models.Client satisfies it; test fakes need
// not, and fall back to plain Chat.
type SlotChatClient interface {
	ChatSlot(model string, messages []models.ChatMessage, idSlot int) (string, error)
}

// chatModel sends a model call, pinning it to the chat slot when one is
// configured and the injected client supports slot pinning. Chat traffic
// (classify and reply) stays off the work slot so a running round is never
// queued behind a chat request, and Nathan's reply is never queued behind
// the round's long request.
func (a *Agent) chatModel(model string, messages []models.ChatMessage) (string, error) {
	a.mu.Lock()
	oc := a.ollama
	slot := a.chatSlot
	a.mu.Unlock()

	if slot > 0 {
		if sc, ok := oc.(SlotChatClient); ok {
			return sc.ChatSlot(model, messages, slot)
		}
	}
	return oc.Chat(model, messages)
}
