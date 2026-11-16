package planworker

import (
	"fmt"
	"html"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
)

// ParseTaskCard turns a sub-item's description back into a Task — the
// inverse of taskDescriptionHTML (B4). C1's execution loop reads its card
// from the board this way instead of trusting the queue payload: the card
// on the board is the source of truth, and a human can edit it between
// planning and execution.
//
// The card's title is the item's name on the board, not part of the
// description — the returned Task has Title empty. Description and the
// acceptance check are required (a card without either cannot be run);
// Files, Dependencies, Size and ExecutorModel are optional.
func ParseTaskCard(desc string) (*Task, error) {
	id, ok := markerValue(desc, planTaskMarker)
	if !ok {
		return nil, fmt.Errorf("not a task card: no %q marker", strings.TrimSuffix(planTaskMarker, " "))
	}
	t := &Task{ID: id}
	v, ok := cardParagraph(desc, "What to build:")
	if !ok {
		return nil, fmt.Errorf("task card %s: no What to build", id)
	}
	t.Description = html.UnescapeString(strings.TrimSpace(v))
	if t.Description == "" {
		return nil, fmt.Errorf("task card %s: empty What to build", id)
	}
	if v, ok := cardParagraph(desc, "Files:"); ok {
		t.Files = splitCardList(v)
	}
	if v, ok := cardParagraph(desc, "Blocked by:"); ok {
		t.Dependencies = splitCardList(v)
	}
	// Capability, when present, names the capability worker that does this
	// task's work instead of an LLM round (plan §3.7). It is a single
	// value (a dotted name), not a comma list.
	if v, ok := cardParagraph(desc, "Capability:"); ok {
		t.Capability = html.UnescapeString(strings.TrimSpace(v))
	}
	v, ok = cardParagraph(desc, "Acceptance check:")
	if !ok {
		return nil, fmt.Errorf("task card %s: no Acceptance check", id)
	}
	t.AcceptanceCheck = html.UnescapeString(strings.TrimSpace(v))
	if t.AcceptanceCheck == "" {
		return nil, fmt.Errorf("task card %s: empty Acceptance check", id)
	}
	// Size and Executor share one paragraph:
	//   <strong>Size:</strong> S &middot; <strong>Executor:</strong> model:<id>
	// — the Size value runs to the Executor tag, the Executor value to </p>.
	if v, ok := cardParagraph(desc, "Size:"); ok {
		// The "S · model:..." separator decodes to a middle dot — drop it.
		t.Size = strings.TrimRight(
			html.UnescapeString(strings.TrimSpace(strings.SplitN(v, "<strong>", 2)[0])),
			" \t·")
	}
	if v, ok := cardParagraph(desc, "Executor:"); ok {
		v = html.UnescapeString(strings.TrimSpace(v))
		v = strings.TrimPrefix(v, placement.ModelPrefix)
		t.ExecutorModel = v
	}
	return t, nil
}

// cardParagraph returns the raw (still HTML-escaped) content of the card
// paragraph that opens with "<strong>label</strong>", up to its closing
// </p>. The labels are written verbatim by taskDescriptionHTML; values are
// html.EscapeString-ed, so a value cannot contain a raw "<strong>" or
// "</p>" and the cut points are unambiguous.
func cardParagraph(desc, label string) (string, bool) {
	needle := "<strong>" + label + "</strong>"
	i := strings.Index(desc, needle)
	if i < 0 {
		return "", false
	}
	rest := desc[i+len(needle):]
	end := strings.Index(rest, "</p>")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// splitCardList splits a "a, b" card list into trimmed non-empty parts.
func splitCardList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = html.UnescapeString(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
