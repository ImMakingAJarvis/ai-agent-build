// blockedit.go: search-and-replace block edits (ELR-1, M3.7a, card
// ELR-36). A second way to express an edit, as plain text, for when the
// model fumbles the JSON of edit_file. One or more blocks:
//
//	<path on its own line>
//	<<<<<<< SEARCH
//	<old lines>
//	=======
//	<new lines>
//	>>>>>>> REPLACE
//
// An empty SEARCH part creates the file with the REPLACE part. Every
// path goes through the perm.Guard like the other file tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

const (
	searchMarker  = "<<<<<<< SEARCH"
	separatorLine = "======="
	replaceMarker = ">>>>>>> REPLACE"
)

// BlockEdit is one parsed search/replace block: the target path and the
// old text to find ("" = create the file) and the text to put there.
type BlockEdit struct {
	Path string
	Old  string
	New  string
}

// ParseBlockEdits parses one or more search/replace blocks out of text.
// The path of a block is the last non-empty line before its SEARCH
// marker, with backticks and surrounding spaces stripped. A SEARCH
// marker with no separator or no REPLACE marker is an error naming the
// block number (blocks are counted from 1, in order of appearance).
func ParseBlockEdits(text string) ([]BlockEdit, error) {
	lines := strings.Split(text, "\n")
	var blocks []BlockEdit
	i := 0
	for i < len(lines) {
		marker := indexOf(lines, i, searchMarker)
		if marker < 0 {
			break
		}
		no := len(blocks) + 1
		path := ""
		for k := marker - 1; k >= i; k-- {
			if s := strings.TrimSpace(lines[k]); s != "" {
				path = strings.TrimSpace(strings.Trim(s, "`"))
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("block %d: no path on the line before the SEARCH marker", no)
		}
		sep := indexOfAny(lines, marker+1, searchMarker, separatorLine)
		if sep < 0 {
			return nil, fmt.Errorf("block %d: SEARCH marker without =======", no)
		}
		end := indexOfAny(lines, sep+1, searchMarker, replaceMarker)
		if end < 0 {
			return nil, fmt.Errorf("block %d: missing %s marker", no, replaceMarker)
		}
		blocks = append(blocks, BlockEdit{
			Path: path,
			Old:  strings.Join(lines[marker+1:sep], "\n"),
			New:  strings.Join(lines[sep+1:end], "\n"),
		})
		i = end + 1
	}
	if len(blocks) == 0 {
		return nil, errors.New("no search/replace blocks (want a path line, then " +
			"<<<<<<< SEARCH, =======, >>>>>>> REPLACE)")
	}
	return blocks, nil
}

// indexOf returns the index of the first line at or after from whose
// trimmed form equals want, or -1.
func indexOf(lines []string, from int, want string) int {
	for j := from; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == want {
			return j
		}
	}
	return -1
}

// indexOfAny is indexOf over several markers at once.
func indexOfAny(lines []string, from int, wants ...string) int {
	for j := from; j < len(lines); j++ {
		s := strings.TrimSpace(lines[j])
		for _, w := range wants {
			if s == w {
				return j
			}
		}
	}
	return -1
}

// ApplyBlockEdit applies one block to the file e.Path, confined by g.
// The exact-match rule of edit_file: Old must occur exactly once. When
// Old occurs 0 times the error lists up to 3 candidate windows of the
// file, best line-match first, so the model can correct the old text.
// An empty Old creates the file (parents included) with New.
func ApplyBlockEdit(g *perm.Guard, e BlockEdit) (string, error) {
	if e.Path == "" {
		return "", errors.New("block edit has no path")
	}
	abs, reason, blocked := g.CheckPath(e.Path)
	if blocked {
		return "", fmt.Errorf("BLOCKED: %s", reason)
	}
	if e.Old == "" {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(abs, []byte(e.New), 0o644); err != nil {
			return "", err
		}
		return "created " + e.Path, nil
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	text := string(content)
	n := strings.Count(text, e.Old)
	switch {
	case n == 1:
		updated := strings.Replace(text, e.Old, e.New, 1)
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return "edited " + e.Path, nil
	case n > 1:
		return "", fmt.Errorf("old text occurs %d times; add more context", n)
	default:
		return "", notFoundError(e.Path, text, e.Old)
	}
}

// notFoundError builds the 0-occurrence error: up to 3 candidate
// windows, each scored by how many of Old's lines (trimmed of spaces)
// match the window's lines positionally.
func notFoundError(path, content, old string) error {
	lines := strings.Split(content, "\n")
	oldLines := strings.Split(old, "\n")
	type cand struct {
		start int // 0-based first line
		score int
	}
	var cands []cand
	for i := 0; i < len(lines); i++ {
		score := 0
		for j, ol := range oldLines {
			if i+j >= len(lines) {
				break
			}
			if strings.TrimSpace(lines[i+j]) == strings.TrimSpace(ol) {
				score++
			}
		}
		if score > 0 {
			cands = append(cands, cand{i, score})
		}
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].score != cands[b].score {
			return cands[a].score > cands[b].score
		}
		return cands[a].start < cands[b].start
	})
	if len(cands) > 3 {
		cands = cands[:3]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "old text not found in %s", path)
	for _, c := range cands {
		end := c.start + len(oldLines)
		if end > len(lines) {
			end = len(lines)
		}
		fmt.Fprintf(&sb, "\n  line %d (%d of %d lines match):", c.start+1, c.score, len(oldLines))
		for k := c.start; k < end; k++ {
			fmt.Fprintf(&sb, "\n    %s", lines[k])
		}
	}
	return errors.New(sb.String())
}

// ApplyEdits applies every block of a search/replace text in order
// (apply_edits). It stops at the first failure and reports which blocks
// were applied.
type ApplyEdits struct {
	Guard *perm.Guard
}

func (ApplyEdits) Spec() ops.Spec {
	return ops.Spec{
		Name: "apply_edits",
		Description: "Apply search/replace blocks in order: a path line, then " +
			"<<<<<<< SEARCH, old lines, =======, new lines, >>>>>>> REPLACE. " +
			"An empty SEARCH part creates the file.",
		Schema: schema(map[string]any{
			"text": map[string]any{"type": "string", "description": "one or more search/replace blocks"},
		}, "text"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t ApplyEdits) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("apply_edits args: %w", err))
	}
	blocks, err := ParseBlockEdits(a.Text)
	if err != nil {
		return fail(err)
	}
	var applied []string
	for i, e := range blocks {
		if _, err := ApplyBlockEdit(t.Guard, e); err != nil {
			msg := fmt.Sprintf("block %d of %d failed: %v", i+1, len(blocks), err)
			if len(applied) > 0 {
				msg += "; applied before it: " + strings.Join(applied, ", ")
			}
			return ops.Result{Status: agentjob.OpFailed, Error: msg}
		}
		applied = append(applied, e.Path)
	}
	progress()
	return done(fmt.Sprintf("applied %d block(s)", len(applied)))
}
