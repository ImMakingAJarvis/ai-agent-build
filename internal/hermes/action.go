package hermes

import (
	"context"
	"fmt"
)

// Action is a pluggable operation that Hermes can execute.
type Action struct {
	// Key is used for registration and LLM routing.
	Key string
	// Description tells the LLM what this action does.
	Description string
	// RequiresFullAccess, when true, requires the user to have full access.
	RequiresFullAccess bool
	// Run executes the action and returns a status message.
	Run func(ctx context.Context, params map[string]string) (string, error)
}

// ResultText formats an execution result for Discord display.
func ResultText(result *Result, status string) string {
	if result == nil {
		return status
	}
	return fmt.Sprintf("**%s** %s: %s", result.Type, result.ActionName, status)
}
