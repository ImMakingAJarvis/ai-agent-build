package agentjob

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	itemRe   = regexp.MustCompile(`^- \[( |x)\] (.*)$`)
	boldRe   = regexp.MustCompile(`^\*\*(.+?)\*\*`)
	branchRe = regexp.MustCompile("\\(branch `([^`]+)`\\)")
	codeRe   = regexp.MustCompile(`^\*{0,2}([A-Z]\d+(?:\.\d+[a-z]?)?)\b`)
	slugRe   = regexp.MustCompile(`[^a-z0-9]+`)
	docRe    = regexp.MustCompile("`docs/jobs/([^`]+)`")
)

const queuedSection = "Queued jobs"

// readDocSection reads a job doc file and extracts the background paragraph
// (text between H1 and first H2) plus the ## <ID> section for the given job ID.
// Returns the combined instructions and the doc path. If the file or section
// cannot be found, returns empty strings.
func readDocSection(baseDir, docPath, id string) (instructions, doc string) {
	data, err := os.ReadFile(filepath.Join(baseDir, docPath))
	if err != nil {
		return "", ""
	}

	lines := strings.Split(string(data), "\n")
	var background, section strings.Builder
	inBackground := false
	inSection := false
	upperID := strings.ToUpper(id)

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "# "):
			// H1 — title; next non-empty lines are background
			inBackground = true
			inSection = false
			background.Reset()
		case strings.HasPrefix(line, "## "):
			inBackground = false
			secID := strings.TrimPrefix(line, "## ")
			// Match by uppercase ID at the start (e.g. "## J0.9" for id "j0.9")
			if strings.HasPrefix(strings.ToUpper(secID), upperID) {
				inSection = true
				section.Reset()
				section.WriteString(line + "\n")
			} else {
				inSection = false
			}
		case inBackground && strings.TrimSpace(line) != "":
			background.WriteString(line + "\n")
		case inSection:
			section.WriteString(line + "\n")
		}
	}

	// Only return background if we found a matching section
	sec := strings.TrimSpace(section.String())
	if sec == "" {
		return "", ""
	}

	var combined strings.Builder
	bg := strings.TrimSpace(background.String())
	if bg != "" {
		combined.WriteString("Background:\n")
		combined.WriteString(bg + "\n\n")
	}
	combined.WriteString(sec)
	return strings.TrimSpace(combined.String()), docPath
}

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndex(s, " "); i > n/2 {
		s = s[:i]
	}
	return strings.TrimRight(s, "-")
}

type item struct {
	h2, h3, intro, text string
	checked             bool
}

func parse(md string) []item {
	var items []item
	var h2, h3 string
	var intro, cur strings.Builder
	var curItem *item
	flush := func() {
		if curItem != nil {
			curItem.text = strings.TrimSpace(cur.String())
			items = append(items, *curItem)
			curItem = nil
		}
		cur.Reset()
	}
	for _, line := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			flush()
			h2, h3 = strings.TrimPrefix(line, "## "), ""
			intro.Reset()
		case strings.HasPrefix(line, "### "):
			flush()
			h3 = strings.TrimPrefix(line, "### ")
			intro.Reset()
		case itemRe.MatchString(line):
			flush()
			m := itemRe.FindStringSubmatch(line)
			curItem = &item{h2: h2, h3: h3, intro: strings.TrimSpace(intro.String()), checked: m[1] == "x"}
			cur.WriteString(m[2] + "\n")
		case curItem != nil && (strings.HasPrefix(line, " ") || strings.TrimSpace(line) == ""):
			cur.WriteString(line + "\n")
		default:
			flush()
			intro.WriteString(line + "\n")
		}
	}
	flush()
	return items
}

// Plan turns the unchecked items of TASKS.md into agent job specs in this
// order: the queued-jobs section (J0's successors, chained with depends_on),
// then the benchmarks (Phase 2 items mentioning a benchmark and all of
// Phase 2b), then every other unchecked item in file order. skipSections are
// H2 prefixes to leave out (work that is already installed and verified).
// IDs are stable: the job code (d0, q1) for queued jobs, else a slug of
// section and title. Earlier jobs get higher priority and earlier created_at.
func Plan(baseDir, md string, now time.Time, skipSections []string) []Spec {
	var queued, bench, rest []item
	for _, it := range parse(md) {
		if it.checked {
			continue
		}
		skip := false
		for _, p := range skipSections {
			if strings.HasPrefix(it.h2, p) {
				skip = true
			}
		}
		switch {
		case skip:
		case strings.HasPrefix(it.h2, queuedSection):
			queued = append(queued, it)
		case strings.HasPrefix(it.h2, "Phase 2b"),
			strings.HasPrefix(it.h2, "Phase 2 ") && strings.Contains(strings.ToLower(it.text), "benchmark"):
			bench = append(bench, it)
		default:
			rest = append(rest, it)
		}
	}
	var specs []Spec
	seen := map[string]bool{}
	add := func(it item, chain bool) {
		first := strings.SplitN(it.text, "\n", 2)[0]
		title := first
		if m := boldRe.FindStringSubmatch(first); m != nil {
			title = m[1]
		}
		id, code := "", ""
		if m := codeRe.FindStringSubmatch(title); m != nil {
			code = m[1]
		} else if m := codeRe.FindStringSubmatch(it.h3); m != nil && !strings.Contains(it.h3, "-") {
			code = m[1]
		}
		if chain && code != "" {
			id = strings.ToLower(code)
			if !strings.HasPrefix(title, code) {
				title = code + " — " + title
			}
		} else {
			sec := it.h3
			if sec == "" || !chain {
				sec = it.h2
			}
			id = cut(slug(strings.SplitN(sec, " — ", 2)[0]+"-"+title), 60)
		}
		for base, n := id, 2; seen[id]; n++ {
			id = base + "-" + strconv.Itoa(n)
		}
		seen[id] = true
		// For queued jobs referencing a job doc, read the ## <ID> section
		// from that doc file as instructions, instead of the index line.
		instructions := it.text
		if it.intro != "" {
			instructions = "Section context: " + cut(it.intro, 3000) + "\n\n" + instructions
		}
		if dm := docRe.FindStringSubmatch(first); dm != nil && code != "" {
			docPath := "docs/jobs/" + dm[1]
			if sec, doc := readDocSection(baseDir, docPath, strings.ToLower(code)); sec != "" {
				instructions = sec
				sp := Spec{ID: id, Title: cut(title, 120), Instructions: instructions, Doc: doc, Driver: "local", Source: "migrate", Class: ClassBackground}
				if m := branchRe.FindStringSubmatch(it.text); m != nil {
					sp.Branch = m[1]
				}
				if chain && len(specs) > 0 {
					sp.DependsOn = []string{specs[len(specs)-1].ID}
				}
				specs = append(specs, sp)
				return
			}
		}
		sp := Spec{ID: id, Title: cut(title, 120), Instructions: instructions, Driver: "local", Source: "migrate", Class: ClassBackground}
		if m := branchRe.FindStringSubmatch(it.text); m != nil {
			sp.Branch = m[1]
		}
		if chain && len(specs) > 0 {
			sp.DependsOn = []string{specs[len(specs)-1].ID}
		}
		specs = append(specs, sp)
	}
	for _, it := range queued {
		add(it, true)
	}
	for _, it := range bench {
		add(it, false)
	}
	for _, it := range rest {
		add(it, false)
	}
	for i := range specs {
		specs[i].Priority = 1000 - i
		specs[i].CreatedAt = now.Add(time.Duration(i) * time.Millisecond)
	}
	return specs
}
