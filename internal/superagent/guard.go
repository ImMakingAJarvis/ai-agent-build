package superagent

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Guard is the safety layer for the Super Agent (the NoBrakes philosophy:
// fail-safe defaults, the agent degrades to "blocked, try another way" and
// never to "did something destructive"). It confines file tools and the
// shell to the work directory and blocks a small list of catastrophic
// commands. It is checked BEFORE any command runs; a blocked action is
// reported to the model as an observation so it can adapt.
type Guard struct {
	WorkDir   string   // shell cwd and root for path confinement
	ExtraDirs []string // additional roots the file tools may touch
	Disabled  bool     // tests only: skip every check
}

// blockedPatterns are command-level blocks. Order matters only for the
// message: the first match wins.
var blockedPatterns = []struct {
	re    *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\b(reboot|shutdown|halt|poweroff)\b`), "reboot/shutdown is not allowed"},
	{regexp.MustCompile(`\bagent-reboot\b`), "reboot is not allowed"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b`), "filesystem formatting is not allowed"},
	{regexp.MustCompile(`\b(fdisk|parted|wipefs)\b`), "disk partitioning is not allowed"},
	{regexp.MustCompile(`\bdd\b[^|;&]*\bof=/dev/`), "writing to a raw device is not allowed"},
	{regexp.MustCompile(`>{1,2}\s*/dev/(sd|nvme|hd|vd|mmcblk)`), "writing to a raw device is not allowed"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{?\s*:\s*\|\s*:`), "fork bomb is not allowed"},
	{regexp.MustCompile(`\bsudo\b.*\b(rm|mkfs|dd|reboot|shutdown)\b`), "destructive sudo is not allowed"},
}

// CheckCommand vets a shell command. It returns the block reason and true
// when the command must not run.
func (g *Guard) CheckCommand(cmd string) (string, bool) {
	if g.Disabled {
		return "", false
	}
	c := strings.TrimSpace(cmd)
	for _, p := range blockedPatterns {
		if p.re.MatchString(c) {
			return p.reason, true
		}
	}
	// Recursive rm of an absolute path outside the work dir: the classic
	// "rm -rf /home" accident. Relative paths stay allowed — they resolve
	// inside the work dir.
	if rmTarget, ok := recursiveRmOutside(c, g.WorkDir); ok {
		return "recursive delete outside the work dir (" + rmTarget + ") is not allowed", true
	}
	// Never push to main/master directly (repo git rule).
	if strings.Contains(c, "git push") && pushToDefaultBranch(c) {
		return "pushing to main/master is not allowed", true
	}
	return "", false
}

// recursiveRmOutside finds an rm with a recursive flag whose target is an
// absolute path not under root. It reports the first offending target.
func recursiveRmOutside(cmd, root string) (string, bool) {
	fields := strings.Fields(cmd)
	root = filepath.Clean(root)
	for i, f := range fields {
		if f != "rm" {
			continue
		}
		rec := false
		var targets []string
		for _, a := range fields[i+1:] {
			if strings.HasPrefix(a, "-") && strings.ContainsAny(a, "rR") {
				rec = true
			} else if !strings.HasPrefix(a, "-") {
				targets = append(targets, a)
			} else {
				// a flag after the first positional (e.g. "rm -rf a -v b"):
				// stop scanning this rm.
				break
			}
		}
		if !rec {
			continue
		}
		for _, t := range targets {
			t = strings.Trim(t, `"'`)
			if !filepath.IsAbs(t) {
				continue
			}
			if !strings.HasPrefix(filepath.Clean(t), root+string(filepath.Separator)) && filepath.Clean(t) != root {
				return t, true
			}
		}
	}
	return "", false
}

func pushToDefaultBranch(cmd string) bool {
	for _, br := range []string{" main", " master", "origin main", "origin master", ":main", ":master", "HEAD:main", "HEAD:master"} {
		if strings.Contains(cmd, br) {
			return true
		}
	}
	return false
}

// CheckPath confines a file-tool path to the work dir (plus ExtraDirs).
// It returns the cleaned absolute path and a block reason when the path
// escapes the roots.
func (g *Guard) CheckPath(p string) (string, string, bool) {
	if g.Disabled {
		return filepath.Clean(p), "", false
	}
	// Relative paths are relative to the work dir, not the process CWD.
	var abs string
	if filepath.IsAbs(p) {
		abs = p
	} else {
		abs = filepath.Join(g.WorkDir, p)
	}
	abs = filepath.Clean(abs)
	roots := append([]string{g.WorkDir}, g.ExtraDirs...)
	for _, r := range roots {
		r = filepath.Clean(r)
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return abs, "", false
		}
	}
	return "", "path escapes the work dir", true
}
