package dashboard

import (
	"encoding/json"
	"strings"
)

// LiveEvent is one step (or token delta) pushed over the live SSE
// connection. A delta event appends Body to the currently open block of
// the same Kind on the page.
type LiveEvent struct {
	Kind  string `json:"kind"` // thought, command, result, output
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	Delta bool   `json:"delta,omitempty"`
}

// LiveParser turns agent output lines — stream-json events or plain
// text — into LiveEvents in arrival order. It merges token deltas into
// the open content block, and drops the full assistant messages that
// repeat what the deltas already showed.
type LiveParser struct {
	sawDelta  bool
	blockKind string // kind of the content block deltas belong to
	buf       strings.Builder
}

// Feed parses one output line and returns the events it contains.
func (p *LiveParser) Feed(line string) []LiveEvent {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil {
		p.buf.WriteString(line)
		p.buf.WriteByte('\n')
		return nil
	}
	return p.event(ev)
}

// Flush emits any accumulated plain text.
func (p *LiveParser) Flush() []LiveEvent {
	var out []LiveEvent
	if t := strings.TrimSpace(p.buf.String()); t != "" {
		p.buf.Reset()
		out = append(out, LiveEvent{Kind: "output", Title: "Output", Body: t})
	}
	return out
}

func (p *LiveParser) event(ev map[string]any) []LiveEvent {
	var out []LiveEvent
	if p.buf.Len() > 0 {
		out = p.Flush()
	}
	switch typ, _ := ev["type"].(string); typ {
	case "system":
		// Session init; nothing to show.
	case "content_block_start":
		b, _ := ev["content_block"].(map[string]any)
		switch b["type"] {
		case "thinking":
			p.blockKind = "thought"
		case "text":
			p.blockKind = "output"
		default:
			p.blockKind = "" // tool_use: input deltas are not shown
		}
	case "content_block_delta":
		d, _ := ev["delta"].(map[string]any)
		var text string
		if t, ok := d["text"].(string); ok {
			text = t
		} else if t, ok := d["thinking"].(string); ok {
			text = t
		}
		if text == "" || p.blockKind == "" {
			return out
		}
		p.sawDelta = true
		out = append(out, LiveEvent{Kind: p.blockKind, Body: text, Delta: true})
	case "content_block_stop":
		p.blockKind = ""
	case "assistant":
		msg, _ := ev["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var text strings.Builder
		for _, raw := range content {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch b["type"] {
			case "thinking":
				// Streamed token by token when deltas are present.
				if p.sawDelta {
					continue
				}
				if t, _ := b["thinking"].(string); strings.TrimSpace(t) != "" {
					out = append(out, LiveEvent{Kind: "thought", Title: "Thought", Body: t})
				}
			case "text":
				if p.sawDelta {
					continue
				}
				if t, _ := b["text"].(string); strings.TrimSpace(t) != "" {
					text.WriteString(t)
				}
			case "tool_use":
				name, _ := b["name"].(string)
				title := name
				if title == "" {
					title = "Tool"
				}
				out = append(out, LiveEvent{Kind: "command", Title: title, Body: formatToolInput(name, b["input"])})
			}
		}
		if t := strings.TrimSpace(text.String()); t != "" {
			out = append(out, LiveEvent{Kind: "output", Title: "Reply", Body: t})
		}
	case "user":
		msg, _ := ev["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, raw := range content {
			b, ok := raw.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			body := stringifyContent(b["content"])
			if strings.TrimSpace(body) != "" {
				out = append(out, LiveEvent{Kind: "result", Title: "Outcome", Body: body})
			}
		}
	case "result":
		// The final answer was already streamed token by token when
		// deltas are present; the result event would repeat it.
		if !p.sawDelta {
			if r, _ := ev["result"].(string); strings.TrimSpace(r) != "" {
				out = append(out, LiveEvent{Kind: "output", Title: "Result", Body: r})
			}
		}
	}
	return out
}
