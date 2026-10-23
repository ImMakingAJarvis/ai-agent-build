package hermes

import (
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

const classifySystemPrompt = `You are Hermes, the intent classifier for Jarvis.
Your job is to determine if the user wants to converse or have Jarvis perform an action.

Respond with ONLY a JSON object. No other text.

For conversational messages:
{"type": "chat", "reply": "Your conversational reply here."}

For action requests, determine the action and extract parameters:
{"type": "action", "action": "<action_key>", "params": {"key": "value"}}

Available actions:
ACTIONS_PLACEHOLDER

Rules:
- If the user asks a question or makes a statement, it is "chat".
- If the user asks Jarvis to DO something (build, fix, install, check status, start/stop services), it is "action".
- Keep chat replies brief and conversational.
- Extract as many parameters as you can from the user's message for actions.`

// buildClassifyPrompt constructs the full message list for intent classification.
// It fills in the available actions into the system prompt and appends the
// user message.
func buildClassifyPrompt(actions []Action, userMessage string) []models.ChatMessage {
	actionList := buildActionList(actions)
	prompt := strings.ReplaceAll(classifySystemPrompt, "ACTIONS_PLACEHOLDER", actionList)

	return []models.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: userMessage},
	}
}

// buildActionList formats the registered actions into a description block
// for the system prompt.
func buildActionList(actions []Action) string {
	if len(actions) == 0 {
		return "(no actions registered)"
	}
	var b strings.Builder
	for i, a := range actions {
		if i > 0 {
			b.WriteString("\n")
		}
		access := ""
		if a.RequiresFullAccess {
			access = " [full access required]"
		}
		fmt.Fprintf(&b, "- %s: %s%s", a.Key, a.Description, access)
	}
	return b.String()
}
