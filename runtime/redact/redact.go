// Package redact scrubs secrets from tool output before the text is
// stored in the message log, spilled to a .full file, or shown in the
// web UI (card ELR-91). A token printed by a command — an env dump, a
// failed curl, git remote -v — must not end up in any of those places.
//
// A Redactor replaces, with the text "[redacted]", every known secret
// value it was built with and every well-known token pattern (GitHub,
// Anthropic, Plane, AWS, Slack, bearer headers, PEM private key
// blocks). The raw op log file is written by the shell tool directly
// and is NOT redacted here; the cards that serve logs over HTTP pass
// them through Apply.
package redact

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// placeholder is what every redacted secret becomes.
	placeholder = "[redacted]"
	// minKnownLen: known values shorter than this are ignored — a
	// 7-character value would redact ordinary words.
	minKnownLen = 8
	// maxFileBytes: FromDir only reads regular files up to this size
	// (the secrets store keeps one secret per 0600 file).
	maxFileBytes = 4096
)

// tokenPatterns are the well-known token formats a tool may print
// (GitHub, Anthropic, Plane, AWS, Slack). Compiled once; each match is
// replaced whole by "[redacted]".
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`ghs_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`plane_api_[a-f0-9]{32}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
}

// bearerPattern matches "bearer <token>" case-insensitively. Apply keeps
// the word and redacts only the token.
var bearerPattern = regexp.MustCompile(`(?i)(bearer)\s+[A-Za-z0-9._~+/-]{20,}=*`)

// privateKeyPattern matches a whole PEM private key block, from the
// BEGIN line to the matching END line.
var privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// Redactor replaces known secret values and token patterns with
// "[redacted]". A nil *Redactor is a no-op (Apply returns its input
// unchanged).
type Redactor struct {
	// values are the known secret strings, sorted longest first so a
	// value that is a prefix of another is replaced whole.
	values []string
}

// New returns a Redactor for the given known secret values. Values
// shorter than minKnownLen characters are ignored; the rest are kept
// longest first.
func New(values []string) *Redactor {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= minKnownLen {
			kept = append(kept, v)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })
	return &Redactor{values: kept}
}

// FromEnv returns the values of the variables in environ (os.Environ
// form, "NAME=value") whose NAME contains TOKEN, KEY, SECRET, PASSWORD
// or PASS (case-insensitive).
func FromEnv(environ []string) []string {
	var values []string
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		u := strings.ToUpper(name)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "KEY") ||
			strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD") ||
			strings.Contains(u, "PASS") {
			values = append(values, val)
		}
	}
	return values
}

// FromDir returns the trimmed content of every regular file in dir that
// is at most maxFileBytes bytes (the secrets store keeps one secret per
// 0600 file). A missing dir returns nil.
func FromDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var values []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > maxFileBytes {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		values = append(values, strings.TrimSpace(string(b)))
	}
	return values
}

// Apply returns s with every known value and every token pattern
// replaced by "[redacted]" (a bearer header keeps the word). A nil
// *Redactor returns s unchanged.
func (r *Redactor) Apply(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, placeholder)
	}
	for _, p := range tokenPatterns {
		s = p.ReplaceAllString(s, placeholder)
	}
	s = bearerPattern.ReplaceAllString(s, "$1 "+placeholder)
	s = privateKeyPattern.ReplaceAllString(s, placeholder)
	return s
}
