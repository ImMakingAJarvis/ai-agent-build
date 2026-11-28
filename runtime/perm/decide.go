package perm

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"
)

// Decision is the outcome of Decide: run the call, refuse it, or park it
// for a human answer. Text in a call's arguments can never change the
// decision — Decide reads only the rules and the call's tool, path and
// command (card ELR-101, edge case 2026-11-24).
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Rule is one {tool, pattern} pair from the permission config (card
// M4.2b feeds these into Rules). Pattern is a path.Match glob.
type Rule struct {
	Tool    string
	Pattern string
}

// Rules is the full permission set: the deny list is checked first, the
// allow list only after the workspace and guard checks.
type Rules struct {
	Allow []Rule
	Deny  []Rule
}

// ToolCall is the call being decided. Args is the raw JSON argument
// object; only the path argument (file tools) and the command string
// (shell) are read, and only as strings — nothing else in Args can
// grant permission. Workspace is the task's worktree root (card M4.2c).
type ToolCall struct {
	Tool      string
	Args      json.RawMessage
	Workspace string
}

// Decide turns (rules, call) into a decision. It is pure: no env, no
// clock, no filesystem, no network — the only guard it consults is
// CheckCommand, which is string-only.
//
// Order (first hit wins):
//
//  1. a Deny rule that matches -> Deny
//  2. a shell call the Guard rooted at call.Workspace refuses -> Deny
//  3. a file tool whose path is inside call.Workspace (filepath.Clean,
//     no filesystem access) -> Allow; a shell call the Guard accepts
//     (inside the workspace) -> Allow
//  4. an Allow rule that matches -> Allow
//  5. everything else — unknown tools, any path outside the workspace
//     or with a .. escape -> Ask
func Decide(rules Rules, call ToolCall) Decision {
	for _, r := range rules.Deny {
		if matchRule(r, call) {
			return Deny
		}
	}

	switch call.Tool {
	case "shell":
		cmd, ok := argString(call.Args, "command")
		if !ok {
			break // no command to check: the rules below decide
		}
		if _, blocked := (&Guard{WorkDir: call.Workspace}).CheckCommand(cmd); blocked {
			return Deny
		}
		return Allow
	case "read_file", "write_file", "edit_file":
		if p, ok := argString(call.Args, "path"); ok && insideWorkspace(call.Workspace, p) {
			return Allow
		}
	}

	for _, r := range rules.Allow {
		if matchRule(r, call) {
			return Allow
		}
	}
	return Ask
}

// argString pulls one string field out of the raw JSON args. Malformed
// JSON, a missing key or a non-string value all report ok=false — the
// call then decides as if it carried no path or command at all.
func argString(args json.RawMessage, key string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok && v != ""
}

// matchRule reports whether r applies to call: the tool names must be
// equal and r.Pattern (a path.Match glob) must match the call's path
// argument (file tools) or shell command string. A call with neither
// target — an unknown tool or a missing argument — matches no rule, and
// a malformed pattern never matches.
func matchRule(r Rule, call ToolCall) bool {
	if r.Tool != call.Tool {
		return false
	}
	var target string
	switch call.Tool {
	case "shell":
		cmd, ok := argString(call.Args, "command")
		if !ok {
			return false
		}
		target = cmd
	case "read_file", "write_file", "edit_file":
		p, ok := argString(call.Args, "path")
		if !ok {
			return false
		}
		target = p
	default:
		return false
	}
	ok, err := path.Match(r.Pattern, target)
	return err == nil && ok
}

// insideWorkspace reports whether p lies inside workspace using string
// operations only: no filesystem access, no symlink resolution (that is
// CheckPath's job; Decide must stay pure). Relative paths resolve
// against the workspace, the same convention as Guard.CheckPath.
func insideWorkspace(workspace, p string) bool {
	var abs string
	if filepath.IsAbs(p) {
		abs = p
	} else {
		abs = filepath.Join(workspace, p)
	}
	abs = filepath.Clean(abs)
	root := filepath.Clean(workspace)
	if root == "" {
		root = "."
	}
	return abs == root || strings.HasPrefix(abs, root+string(filepath.Separator))
}
