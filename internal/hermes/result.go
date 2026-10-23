package hermes

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Result type constants.
const (
	TypeChat   = "chat"
	TypeAction = "action"
)

// Result is the structured response from the LLM intent classifier.
type Result struct {
	Type       string            `json:"type"`       // chat or action
	Reply      string            `json:"reply"`      // LLM reply for chat type
	ActionName string            `json:"action"`     // action key for action type
	Parameters map[string]string `json:"params"`     // free-form params for the action
}

// parseResult extracts a Result from the raw LLM response.
// The LLM is prompted to return JSON; this function strips any
// surrounding markdown code fences and unmarshals the result.
func parseResult(raw string) (*Result, error) {
	trimmed := strings.TrimSpace(raw)

	// Strip markdown code fences if present.
	if strings.HasPrefix(trimmed, "```") {
		// Find the closing fence.
		idx := strings.Index(trimmed[3:], "```")
		if idx < 0 {
			return nil, fmt.Errorf("hermes: unclosed code fence in response")
		}
		trimmed = trimmed[3 : 3+idx]
		// Strip optional language tag after opening fence.
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			trimmed = trimmed[i+1:]
		}
	}

	var r Result
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return nil, fmt.Errorf("hermes: invalid JSON in response: %w", err)
	}

	if r.Type != TypeChat && r.Type != TypeAction {
		return nil, fmt.Errorf("hermes: unknown result type %q", r.Type)
	}

	return &r, nil
}
