// Package tools is the runtime's core file tools (ELR-1, M3.2b, card
// ELR-26): read_file, write_file, edit_file, list and search, as
// ops.Tool implementations. They copy the behaviour and limits of the
// super agent's file tools (internal/superagent/tools.go) and confine
// every path through the perm.Guard (card ELR-25): a refused path is a
// failed result with "BLOCKED: <reason>", never an error the Runner
// would have to special-case.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

const (
	defaultReadLimit  = 2000    // read_file default line count
	defaultSearchMax  = 100     // search default match count
	maxSearchFileSize = 1 << 20 // search skips files over 1 MB
)

// schema builds a tool's JSON Schema argument object: type object, the
// named properties, and the required keys.
func schema(properties map[string]any, required ...string) json.RawMessage {
	obj := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		obj["required"] = required
	}
	b, _ := json.Marshal(obj)
	return b
}

// blocked is the result for a path the Guard refused.
func blockedResult(reason string) ops.Result {
	return ops.Result{Status: agentjob.OpFailed, Error: "BLOCKED: " + reason}
}

// fail reports a tool-level error as a failed result.
func fail(err error) ops.Result {
	return ops.Result{Status: agentjob.OpFailed, Error: err.Error()}
}

// done reports a finished tool with its output.
func done(output string) ops.Result {
	return ops.Result{Status: agentjob.OpDone, Output: output}
}

// ReadFile reads a file with line numbers (read_file).
type ReadFile struct {
	Guard *perm.Guard
}

func (ReadFile) Spec() ops.Spec {
	return ops.Spec{
		Name:        "read_file",
		Description: "Read a file with line numbers (1-based offset, default limit 2000 lines)",
		Schema: schema(map[string]any{
			"path":   map[string]any{"type": "string", "description": "file path, relative to the work dir"},
			"offset": map[string]any{"type": "integer", "description": "first line to read, 1-based (default 1)"},
			"limit":  map[string]any{"type": "integer", "description": "max lines to read (default 2000)"},
		}, "path"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t ReadFile) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("read_file args: %w", err))
	}
	if a.Path == "" {
		return fail(errors.New("read_file needs a path"))
	}
	abs, reason, blocked := t.Guard.CheckPath(a.Path)
	if blocked {
		return blockedResult(reason)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return fail(err)
	}
	lines := strings.Split(string(b), "\n")
	off := a.Offset
	if off < 1 {
		off = 1
	}
	limit := a.Limit
	if limit < 1 {
		limit = defaultReadLimit
	}
	var sb strings.Builder
	end := off + limit - 1
	for i := off - 1; i < len(lines) && i < end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, lines[i])
	}
	if end > len(lines) {
		fmt.Fprintf(&sb, "(file has %d lines)", len(lines))
	}
	progress()
	return done(sb.String())
}

// WriteFile creates or overwrites a file (write_file), making parent
// directories.
type WriteFile struct {
	Guard *perm.Guard
}

func (WriteFile) Spec() ops.Spec {
	return ops.Spec{
		Name:        "write_file",
		Description: "Create or overwrite a file in the work dir (parent directories are created)",
		Schema: schema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "file path, relative to the work dir"},
			"content": map[string]any{"type": "string", "description": "full file content"},
		}, "path", "content"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t WriteFile) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("write_file args: %w", err))
	}
	if a.Path == "" {
		return fail(errors.New("write_file needs a path"))
	}
	abs, reason, blocked := t.Guard.CheckPath(a.Path)
	if blocked {
		return blockedResult(reason)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return fail(err)
	}
	progress()
	return done(fmt.Sprintf("wrote %d bytes", len(a.Content)))
}

// EditFile replaces exactly one occurrence of old with new (edit_file).
// It refuses an ambiguous edit instead of guessing which occurrence the
// caller meant.
type EditFile struct {
	Guard *perm.Guard
}

func (EditFile) Spec() ops.Spec {
	return ops.Spec{
		Name:        "edit_file",
		Description: "Replace exactly one occurrence of old with new in a file",
		Schema: schema(map[string]any{
			"path": map[string]any{"type": "string", "description": "file path, relative to the work dir"},
			"old":  map[string]any{"type": "string", "description": "exact text to replace (must occur exactly once)"},
			"new":  map[string]any{"type": "string", "description": "replacement text"},
		}, "path", "old", "new"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t EditFile) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("edit_file args: %w", err))
	}
	if a.Path == "" {
		return fail(errors.New("edit_file needs a path"))
	}
	abs, reason, blocked := t.Guard.CheckPath(a.Path)
	if blocked {
		return blockedResult(reason)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return fail(err)
	}
	switch n := strings.Count(string(content), a.Old); {
	case n == 0:
		return ops.Result{Status: agentjob.OpFailed, Error: "old text not found"}
	case n > 1:
		return ops.Result{Status: agentjob.OpFailed,
			Error: fmt.Sprintf("old text occurs %d times; add more context", n)}
	}
	updated := strings.Replace(string(content), a.Old, a.New, 1)
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return fail(err)
	}
	progress()
	return done("edited " + a.Path)
}

// List shows the entries of a directory (list), directories with a
// trailing slash, sorted.
type List struct {
	Guard *perm.Guard
}

func (List) Spec() ops.Spec {
	return ops.Spec{
		Name:        "list",
		Description: "List a directory (directories with a trailing /), sorted",
		Schema: schema(map[string]any{
			"path": map[string]any{"type": "string", "description": "directory path, relative to the work dir (default .)"},
		}, "path"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t List) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("list args: %w", err))
	}
	if a.Path == "" {
		a.Path = "."
	}
	abs, reason, blocked := t.Guard.CheckPath(a.Path)
	if blocked {
		return blockedResult(reason)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return fail(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	progress()
	if len(names) == 0 {
		return done("(empty directory)")
	}
	return done(strings.Join(names, "\n"))
}

// Search greps the files under a directory for a Go regexp pattern
// (search). It skips .git directories and files that are over 1 MB or
// look binary (a NUL byte in the first 512 bytes) and reports at most
// max lines, each as "path:line: text".
type Search struct {
	Guard *perm.Guard
}

func (Search) Spec() ops.Spec {
	return ops.Spec{
		Name:        "search",
		Description: "Search file contents for a Go regexp pattern (skips .git and binary files)",
		Schema: schema(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Go regexp matched against each line"},
			"path":    map[string]any{"type": "string", "description": "directory to walk (default the work dir)"},
			"max":     map[string]any{"type": "integer", "description": "max matching lines to report (default 100)"},
		}, "pattern"),
		Class:      ops.Quick,
		ExpectFast: true,
	}
}

func (t Search) Run(_ context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Max     int    `json:"max"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("search args: %w", err))
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return fail(fmt.Errorf("search pattern: %w", err))
	}
	if a.Path == "" {
		a.Path = t.Guard.WorkDir
	}
	abs, reason, blocked := t.Guard.CheckPath(a.Path)
	if blocked {
		return blockedResult(reason)
	}
	max := a.Max
	if max < 1 {
		max = defaultSearchMax
	}
	var out []string
	stop := errors.New("search: max reached")
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it
		}
		if d.IsDir() {
			if p != abs && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= max {
			return stop
		}
		if skippable(p) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s", p, i+1, line))
				if len(out) >= max {
					return stop
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return fail(err)
	}
	progress()
	if len(out) == 0 {
		return done("(no matches)")
	}
	return done(strings.Join(out, "\n"))
}

// skippable reports whether p is over 1 MB or looks binary (a NUL byte
// in the first 512 bytes).
func skippable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.Size() > maxSearchFileSize {
		return true
	}
	f, err := os.Open(p)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// FileTools returns the five core file tools, all confined by g.
func FileTools(g *perm.Guard) []ops.Tool {
	return []ops.Tool{
		ReadFile{Guard: g},
		WriteFile{Guard: g},
		EditFile{Guard: g},
		List{Guard: g},
		Search{Guard: g},
	}
}
