package agentjob

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	itemRe   = regexp.MustCompile(`^- \[( |x)\] (.*)$`)
	boldRe   = regexp.MustCompile(`^\*\*(.+?)\*\*`)
	branchRe = regexp.MustCompile("\\(branch `([^`]+)`\\)")
	codeRe   = regexp.MustCompile(`^\*{0,2}([A-Z]\d+)\b`)
	slugRe   = regexp.MustCompile(`[^a-z0-9]+`)
)

const queuedSection = "Queued jobs"

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
func Plan(md string, now time.Time, skipSections []string) []Spec {
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
		text := it.text
		if it.intro != "" {
			text = "Section context: " + cut(it.intro, 3000) + "\n\n" + text
		}
		sp := Spec{ID: id, Title: cut(title, 120), Instructions: text, Driver: "local", Source: "migrate"}
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
