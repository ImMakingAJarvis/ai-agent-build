package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// slotChatRequest is the /v1/chat/completions body for a request pinned to
// a llama.cpp parallel slot. id_slot is a llama.cpp server extension: the
// request is served only by that slot, never by the work slot.
type slotChatRequest struct {
	Model              string         `json:"model"`
	Messages           []ChatMessage  `json:"messages"`
	Stream             bool           `json:"stream"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	IDSlot             int            `json:"id_slot"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

// chatSlotMaxTokens caps chat-slot replies. Chat answers are short; the cap
// also covers the model's thinking tokens.
const chatSlotMaxTokens = 1024

// ChatSlot is Chat pinned to a llama.cpp parallel slot (id_slot). Chat
// traffic — replies to Nathan — uses this so it runs on the reserved chat
// slot and never queues behind the work slot's long requests, and the
// work slot's round is never delayed by a chat reply.
//
// A zero or negative idSlot disables pinning (any idle slot).
func (c *Client) ChatSlot(model string, messages []ChatMessage, idSlot int) (string, error) {
	req := slotChatRequest{
		Model:     model,
		Messages:  messages,
		Stream:    false,
		MaxTokens: chatSlotMaxTokens,
		IDSlot:    idSlot,
		// Voice/Discord chat: thinking off so replies stay short.
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal slot chat request: %w", err)
	}

	resp, err := c.http.Post(c.baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /v1/chat/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("/v1/chat/completions returned %d: %s", resp.StatusCode, b)
	}

	var cr ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("chat response had no choices")
	}
	return cr.Choices[0].Message.Content, nil
}
