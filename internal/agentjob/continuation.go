package agentjob

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Continuation is the machine-readable record of a yielded job —
// jobs/<id>/continuation.json, written by `jarvis-jobs yield` (plan §3.5,
// EVB-13). Its human/LLM-readable twin is continuation.md in the same
// directory; together they are the durable per-job record that replaces the
// single global notes/brain-handoff.md. The switchboard (plan task 14)
// fills ListenerIDs as it attaches listeners to the reply queue and updates
// ResultRefs as results arrive (via RecordResult).
type Continuation struct {
	JobID string `json:"job_id"`
	// Goal: what the job is for (the job's title at yield).
	Goal string `json:"goal"`
	// WaitOn: the correlation ids the job is parked on — the results it
	// waits for on its reply queue (mirrors Spec.WaitOn at yield).
	WaitOn []string `json:"wait_on"`
	// WaitUntil: the wait deadline, RFC3339 (invariant 2 — no job waits
	// forever).
	WaitUntil string `json:"wait_until"`
	// ListenerIDs: the switchboard's listener ids watching the WaitOn
	// queues. Empty until the switchboard attaches them (plan task 14).
	ListenerIDs []string `json:"listener_ids,omitempty"`
	// ResultRefs: corr -> ref for the results that have arrived so far.
	// RecordResult keeps this in step with the results.json sidecar.
	ResultRefs map[string]string `json:"result_refs,omitempty"`
	// RoundCount: how many rounds the job has run in this continuation
	// chain — 1 at the first yield, incremented on every re-yield after a
	// wake.
	RoundCount int `json:"round_count"`
	// LastDriver: the driver of the round that yielded.
	LastDriver string `json:"last_driver,omitempty"`
	// SessionID: the Claude Code session id of the yielding round, so a
	// future resume can re-attach it. Empty until agent-run records it
	// (plan tasks 16/18).
	SessionID string `json:"session_id,omitempty"`
	// Note: the yield note — the round's message for its successor
	// (mirrors State.WaitNote).
	Note string `json:"note,omitempty"`
	// At: when this continuation was written, RFC3339.
	At string `json:"at"`
}

// Continuation.md section headers, in scaffold order. The headers are
// canonical: the fill check (ContinuationMDFilled) and the woken round both
// key off them, so a continuation written by hand must use the same names.
const (
	SectionGoal    = "Goal"
	SectionDone    = "Done so far"
	SectionFlight  = "In flight (corr ids)"
	SectionResults = "What to do with each result"
	SectionNext    = "Next steps"
	SectionGotchas = "Gotchas"
)

// RenderContinuationMD renders the continuation.md for a yield. The content
// map is already resolved (flags, existing file, prefills — see
// cmd/jarvis-jobs yield); keys are the Section* names and may be empty, in
// which case the section renders with no body.
func RenderContinuationMD(id string, c map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Continuation — %s\n\n", id)
	b.WriteString("<!-- Written by `jarvis-jobs yield` (event-driven brain plan §3.5).\n" +
		"     The woken round reads this file FIRST: what the job is for, what is\n" +
		"     already done, what is in flight, and the exact next step — then it does\n" +
		"     the next step, without re-planning. \"In flight\" and \"Next steps\" must\n" +
		"     carry the round's own words; yield refuses an empty one (invariant 8).\n" +
		"     Machine-readable twin: continuation.json. -->\n\n")
	writeSection := func(name, body string) {
		fmt.Fprintf(&b, "## %s\n", name)
		if body != "" {
			b.WriteString(body)
		}
		b.WriteString("\n")
	}
	writeSection(SectionGoal, c[SectionGoal])
	writeSection(SectionDone, c[SectionDone])
	writeSection(SectionFlight, c[SectionFlight])
	writeSection(SectionResults, c[SectionResults])
	writeSection(SectionNext, c[SectionNext])
	writeSection(SectionGotchas, c[SectionGotchas])
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// ParseContinuationMD splits a continuation.md into its sections: canonical
// section name -> trimmed body. A section whose header is missing (or was
// renamed) is absent from the map. Header matching is by prefix, the same
// rule as ContinuationMDFilled.
func ParseContinuationMD(md string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{SectionGoal, SectionDone, SectionFlight, SectionResults, SectionNext, SectionGotchas} {
		var b strings.Builder
		for _, l := range sectionBody(md, sectionKey(key)) {
			b.WriteString(l + "\n")
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			out[key] = s
		}
	}
	return out
}

// sectionKey is the header prefix used to find a canonical section: the
// name itself, except "In flight (corr ids)" is matched by the shorter
// "In flight" prefix, so both the rendered full header and a round that
// hand-wrote the shorter one are found.
func sectionKey(name string) string {
	if name == SectionFlight {
		return "In flight"
	}
	return name
}

// sectionBody returns the lines under the first "## " header whose text
// starts with name (case-sensitive), up to the next "## " header or EOF.
func sectionBody(md, name string) []string {
	var (
		out   []string
		inSec bool
	)
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## ") {
			text := strings.TrimPrefix(line, "## ")
			if inSec {
				break
			}
			inSec = strings.HasPrefix(text, name)
			continue
		}
		if inSec {
			out = append(out, line)
		}
	}
	return out
}

// ContinuationMDFilled reports whether the mandatory sections of a
// continuation.md carry real content: non-blank lines under "In flight"
// (matched by prefix — the full header is "In flight (corr ids)") and under
// "Next steps". This is the invariant-8 check: yield refuses to flip the
// status to waiting until both hold.
func ContinuationMDFilled(md string) (inFlight, next bool) {
	filled := func(name string) bool {
		for _, l := range sectionBody(md, sectionKey(name)) {
			if strings.TrimSpace(l) != "" {
				return true
			}
		}
		return false
	}
	return filled(SectionFlight), filled(SectionNext)
}

// ContinuationMDPath is the path of a job's continuation.md.
func (s *Store) ContinuationMDPath(id string) string {
	return filepath.Join(s.JobDir(id), "continuation.md")
}

// ContinuationJSONPath is the path of a job's continuation.json.
func (s *Store) ContinuationJSONPath(id string) string {
	return filepath.Join(s.JobDir(id), "continuation.json")
}

// WriteContinuation writes the continuation.json sidecar.
func (s *Store) WriteContinuation(c *Continuation) error {
	return writeJSON(s.ContinuationJSONPath(c.JobID), c)
}

// ReadContinuation loads the continuation.json sidecar. It returns
// os.ErrNotExist when the job has no continuation (never yielded).
func (s *Store) ReadContinuation(id string) (*Continuation, error) {
	b, err := os.ReadFile(s.ContinuationJSONPath(id))
	if err != nil {
		return nil, err
	}
	var c Continuation
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// RecordedResults returns the results that have arrived for a job so far —
// the results.json sidecar, empty when none have.
func (s *Store) RecordedResults(id string) (map[string]Result, error) {
	return s.results(id)
}

// MissingResults lists the WaitOn correlation ids with no recorded result
// yet — what a waiting job is still waiting on.
func (s *Store) MissingResults(id string) ([]string, error) {
	j, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	res, err := s.results(id)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, corr := range j.WaitOn {
		if _, ok := res[corr]; !ok {
			missing = append(missing, corr)
		}
	}
	return missing, nil
}

// WaitAge renders the wait age of a waiting job as a short string — "45s",
// "12m", "2h3m", "2d2h" (the same buckets the dashboard's waiting row uses).
// An empty or unparseable WaitingSince renders as "?".
func WaitAge(j Job, now time.Time) string {
	if j.WaitingSince == "" {
		return "?"
	}
	t, err := time.Parse(time.RFC3339, j.WaitingSince)
	if err != nil {
		return "?"
	}
	return waitDurationString(now.Sub(t))
}

// WaitRemaining renders the time left until a waiting job's deadline:
// "in 95m", "in 2h3m", or "OVERDUE by 5m" when the deadline has passed.
// A job with no deadline renders "no deadline" — an invariant-2 violation
// callers should treat as a bug.
func WaitRemaining(j Job, now time.Time) string {
	dl := j.WaitDeadline()
	if dl.IsZero() {
		return "no deadline"
	}
	d := dl.Sub(now)
	if d <= 0 {
		return "OVERDUE by " + waitDurationString(-d)
	}
	return "in " + waitDurationString(d)
}

// waitDurationString buckets a duration: "45s", "12m", "2h3m", "2d2h".
func waitDurationString(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
