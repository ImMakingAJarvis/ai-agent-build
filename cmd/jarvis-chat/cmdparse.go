// Shared parsing for the owner's !commands.
//
// The NoBrakes switches have two spellings: the short form the CLI and
// the checkpoint buttons use (!automode, !fullauto, !keepgoing) and the
// underscored form the owner tends to type (!auto_mode, !full_auto,
// !keep_going). Each cut function passes both spellings to cutCmdPrefix
// so either one reaches the daemon's handler instead of falling through
// to the brain as an ordinary message (which used to get it to queue a
// worker job for a mode change).
package main

import "strings"

// cutCmdPrefix reports whether content starts with one of the given
// command prefixes followed by a word boundary — a space, a tab, or the
// end of the message — and returns the remainder after the prefix. A
// prefix glued to more characters ("!automodex") is not a command, so
// the message falls through to the brain.
func cutCmdPrefix(content string, prefixes ...string) (rest string, ok bool) {
	for _, p := range prefixes {
		trimmed := strings.TrimPrefix(content, p)
		if trimmed != content && (trimmed == "" || trimmed[0] == ' ' || trimmed[0] == '\t') {
			return trimmed, true
		}
	}
	return "", false
}
