// Output spill (ELR-1, M3.3, card ELR-28): a tool result bigger than a
// limit never enters the model's context whole. Spill stores the full
// text through save and returns a summary — head, tail, the
// error-looking middle lines, and the handle the model can read back
// with the search_output tool.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

const (
	spillHeadLines = 40 // first lines kept in a spilled summary
	spillTailLines = 40 // last lines kept in a spilled summary
	spillMidErrors = 30 // error-looking middle lines kept in a spilled summary
	spillLineChars = 500 // a longer line is cut and marked with "…"
	searchMax      = 100 // search_output pattern match cap
	rangeMax       = 200 // search_output line range cap
)

// spillRe picks the error-looking lines out of the middle of a spilled
// output.
var spillRe = regexp.MustCompile(`(?i)error|fail|panic|fatal|exception|traceback`)

// Spilled is the result of spilling one tool output.
type Spilled struct {
	Text    string // what the model sees: the output, or the summary
	Handle  string // the save handle, empty when nothing was spilled
	Spilled bool   // true when the output exceeded the limit
}

// splitLines splits s on "\n"; a single trailing newline does not count
// as a final empty line.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// cutLine shortens a line to at most spillLineChars runes, marked with
// "…".
func cutLine(line string) string {
	r := []rune(line)
	if len(r) <= spillLineChars {
		return line
	}
	return string(r[:spillLineChars]) + "…"
}

// Spill shrinks a big tool output to a context-sized summary. If output
// is at most limitBytes it is returned untouched and save is not
// called. Otherwise save stores the full text and Text is: the header
// line, the first 40 lines, up to 30 error-looking middle lines with
// their line numbers, and the last 40 lines; lines longer than 500
// characters are cut.
func Spill(output string, limitBytes int, save func(content string) (handle string, err error)) (Spilled, error) {
	if len(output) <= limitBytes {
		return Spilled{Text: output}, nil
	}
	handle, err := save(output)
	if err != nil {
		return Spilled{}, err
	}
	lines := splitLines(output)
	n := len(lines)

	headEnd := min(spillHeadLines, n)
	tailStart := n - spillTailLines
	if tailStart < headEnd {
		tailStart = headEnd
	}

	parts := []string{fmt.Sprintf(
		"[output was %d bytes, %d lines; full text saved as %s; use search_output to read more]",
		len(output), n, handle)}
	for i := 0; i < headEnd; i++ {
		parts = append(parts, cutLine(lines[i]))
	}
	mid := []string{}
	for i := headEnd; i < tailStart && len(mid) < spillMidErrors; i++ {
		if spillRe.MatchString(lines[i]) {
			mid = append(mid, fmt.Sprintf("%d: %s", i+1, cutLine(lines[i])))
		}
	}
	if len(mid) > 0 {
		parts = append(parts, "...")
		parts = append(parts, mid...)
		parts = append(parts, "...")
	}
	for i := tailStart; i < n; i++ {
		parts = append(parts, cutLine(lines[i]))
	}
	return Spilled{Text: strings.Join(parts, "\n"), Handle: handle, Spilled: true}, nil
}

// SearchOutput reads back part of a spilled tool output (search_output).
// Open resolves a handle to the full text; the runtime wires it to the
// spill store, the tool never touches the filesystem itself.
type SearchOutput struct {
	Open func(handle string) (string, error)
}

func (SearchOutput) Spec() ops.Spec {
	return ops.Spec{
		Name:        "search_output",
		Description: "Read more of a spilled tool output: a pattern search or a line range of the saved text",
		Schema: schema(map[string]any{
			"handle":  map[string]any{"type": "string", "description": "the handle from the spill header"},
			"pattern": map[string]any{"type": "string", "description": "optional regexp; matching lines with numbers (max 100)"},
			"from":    map[string]any{"type": "integer", "description": "optional first line, 1-based"},
			"to":      map[string]any{"type": "integer", "description": "optional last line, 1-based (max 200 lines)"},
		}, "handle"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

// Run resolves the handle and returns the matching or ranged lines,
// numbered. A handle containing "/" or ".." is refused before Open is
// called.
func (t SearchOutput) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Handle  string `json:"handle"`
		Pattern string `json:"pattern"`
		From    int    `json:"from"`
		To      int    `json:"to"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("search_output args: %w", err))
	}
	if a.Handle == "" {
		return fail(errors.New("search_output needs a handle"))
	}
	if strings.Contains(a.Handle, "/") || strings.Contains(a.Handle, "..") {
		return fail(errors.New("search_output: bad handle"))
	}
	if t.Open == nil {
		return fail(errors.New("search_output: no spill store wired"))
	}
	content, err := t.Open(a.Handle)
	if err != nil {
		return fail(err)
	}
	lines := splitLines(content)
	progress()
	switch {
	case a.Pattern != "":
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return fail(fmt.Errorf("search_output pattern: %w", err))
		}
		out := []string{}
		for i, l := range lines {
			if re.MatchString(l) {
				out = append(out, fmt.Sprintf("%d: %s", i+1, cutLine(l)))
				if len(out) >= searchMax {
					break
				}
			}
		}
		if len(out) == 0 {
			return done("(no matches)")
		}
		return done(strings.Join(out, "\n"))
	case a.From > 0 || a.To > 0:
		from := a.From
		if from < 1 {
			from = 1
		}
		to := a.To
		if to < 1 || to > len(lines) {
			to = len(lines)
		}
		if from > len(lines) || from > to {
			return done("(no lines)")
		}
		out := []string{}
		for i := from - 1; i < to && len(out) < rangeMax; i++ {
			out = append(out, fmt.Sprintf("%d: %s", i+1, cutLine(lines[i])))
		}
		if len(out) == 0 {
			return done("(no lines)")
		}
		return done(strings.Join(out, "\n"))
	default:
		return fail(errors.New("give a pattern or a line range"))
	}
}
