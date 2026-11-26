// guards.go counts the two failure runs of a job's message log (card
// ELR-88, M3.4d): the run of unusable replies at the end (trailingBad)
// and the run of identical tool calls (trailingSame). Both are pure
// functions over the saved messages: Step calls them before a tick and
// fails the job when a run reaches the configured limit.
package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// runtimeNotePrefix marks the user messages the loop itself appends to
// answer an unusable reply ("[runtime] your ..."). The repeat warning
// ("[runtime] you sent ...") deliberately does not start with it, so a
// warning never counts as a bad reply.
const runtimeNotePrefix = "[runtime] your"

// trailingBad counts the unusable replies at the end of the log: the
// user notes starting with runtimeNotePrefix that answer them, and the
// empty assistant messages that are the bad replies themselves (an
// engine error leaves a note without any assistant message at all). A
// usable assistant reply — text or a tool call — ends the run, as does
// any other message (a user message with other text, a tool result, a
// note stored in an unexpected shape).
func trailingBad(msgs []agentjob.LoopMessage) int {
	n := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		switch m.Role {
		case "user":
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil || !strings.HasPrefix(s, runtimeNotePrefix) {
				return n
			}
			n++
		case "assistant":
			var a assistantContent
			if err := json.Unmarshal(m.Content, &a); err != nil {
				return n // a plain answer: a usable reply, the run ends
			}
			if len(a.ToolCalls) > 0 || strings.TrimSpace(a.Content) != "" {
				return n // a usable reply: the run ends
			}
			// an empty assistant message is the bad reply its note
			// answers: it sits inside the run
		default:
			return n
		}
	}
	return n
}

// callSig is one tool call reduced to what the repeat check compares:
// the tool name and the argument bytes. The call ID is not part of the
// signature: a model that resends the same call with a new ID is
// repeating itself.
type callSig struct {
	name string
	args json.RawMessage
}

func callSigs(calls []engine.ToolCall) []callSig {
	out := make([]callSig, len(calls))
	for i, c := range calls {
		out[i] = callSig{name: c.Name, args: c.Arguments}
	}
	return out
}

func sameSigs(a, b []callSig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].name != b[i].name || !bytes.Equal(a[i].args, b[i].args) {
			return false
		}
	}
	return true
}

// trailingSame counts the assistant messages at the end of the log that
// carry exactly the same tool calls (same names, same argument bytes,
// same order). Tool results sit between the calls and are skipped; any
// other message ends the run. An assistant message with no tool call —
// a plain answer — ends the run, so a finished job never counts as
// repeating.
func trailingSame(msgs []agentjob.LoopMessage) int {
	var want []callSig
	n := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "assistant" {
			continue // tool results and runtime notes sit between the calls
		}
		var a assistantContent
		if err := json.Unmarshal(m.Content, &a); err != nil || len(a.ToolCalls) == 0 {
			break // a plain answer or garbage: the run ends
		}
		sig := callSigs(a.ToolCalls)
		if n == 0 {
			want = sig
			n = 1
			continue
		}
		if !sameSigs(sig, want) {
			break
		}
		n++
	}
	return n
}

// repeatWarnNote is the one-time warning note for a run of n identical
// tool calls (card ELR-88). It starts "[runtime] you", not "[runtime]
// your", so it never counts as a bad reply in trailingBad.
func repeatWarnNote(n int) string {
	return fmt.Sprintf("[runtime] you sent the same tool call %d times and got the same result. Do something different.", n)
}

// isRepeatWarn reports whether the last message is already the repeat
// warning note for a run of n identical calls, so Step appends it once
// and not again as the run keeps growing.
func isRepeatWarn(msgs []agentjob.LoopMessage, n int) bool {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		return false
	}
	var s string
	return json.Unmarshal(msgs[len(msgs)-1].Content, &s) == nil && s == repeatWarnNote(n)
}
