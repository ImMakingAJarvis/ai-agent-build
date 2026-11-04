package superagent

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Action is one tool call the executor model emits. The protocol: the
// model may think in plain text, but must end its reply with exactly one
// fenced ACTION block holding a single JSON object.
//
//	<ACTION>
//	{"tool": "shell", "args": {"cmd": "go build ./..."}}
//	</ACTION>
//
// Special tools: step_done (step achieved), replan (plan no longer fits),
// finish (goal done or failed).
type Action struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

var actionRe = regexp.MustCompile("(?s)<ACTION>\\s*(.*?)\\s*</ACTION>")

// ParseAction extracts the last ACTION block from a model reply and parses
// its JSON. The last block wins: a model that rambles and corrects itself
// keeps its correction.
func ParseAction(reply string) (Action, error) {
	m := actionRe.FindAllStringSubmatch(reply, -1)
	if len(m) == 0 {
		return Action{}, fmt.Errorf("no ACTION block in reply")
	}
	var a Action
	if err := json.Unmarshal([]byte(m[len(m)-1][1]), &a); err != nil {
		return Action{}, fmt.Errorf("parse ACTION JSON: %w", err)
	}
	if a.Tool == "" {
		return Action{}, fmt.Errorf("ACTION JSON has no tool")
	}
	return a, nil
}

// argStr returns args[key] as a string, or def when absent.
func argStr(args json.RawMessage, key, def string) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return def
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return def
}

// argInt returns args[key] as an int, or fallback when absent or not numeric.
func argInt(args json.RawMessage, key string, fallback int) int {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return fallback
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
