package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// LogEvent is one chronological step on a job log page.
type LogEvent struct {
	Kind  string `json:"kind"` // thought, command, result, output
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// JobLog is the data for GET /jobs/{id}.
type JobLog struct {
	ID          string
	Title       string
	Status      string
	Driver      string
	Model       string
	PRURL       string
	StartedAt   string
	FinishedAt  string
	Duration    string
	Error       string
	Summary     string
	Events      []LogEvent
	Missing     bool
	ReadError   string
	Empty       bool
	Live        bool
	Truncated   bool
	SourceLabel string
}

func jobLogFuncs() template.FuncMap {
	return template.FuncMap{
		"statusClass":    StatusClass,
		"formatDuration": FormatDuration,
		"formatCost":     FormatCost,
		"formatKwh":      FormatKwh,
		"divCost":        DivCost,
		"slotRuntime":    SlotRuntime,
		"jobPath":        JobPath,
	}
}

// JobPath is the dashboard URL for a job's log.
func JobPath(id string) string {
	if !agentjob.ValidID(id) {
		return "/"
	}
	return "/jobs/" + id
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	page := s.loadJobLog(r.PathValue("id"))
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "job-log.html", page); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if page.Missing {
		status = http.StatusNotFound
	} else if page.ReadError != "" {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) handleJobLogHTML(w http.ResponseWriter, r *http.Request) {
	page := s.loadJobLog(r.PathValue("id"))
	if !page.Missing && page.ReadError == "" {
		// Formatted view is on demand and bounded: parse only the log's tail.
		if st, err := agentjob.Open(filepath.Dir(s.historyPath)); err == nil {
			if raw, label, trunc, err := readJobTail(st, page.ID, maxFormatBytes); err == nil {
				page.Events = ParseTranscript(raw)
				page.SourceLabel = label
				page.Truncated = trunc
				page.Empty = len(page.Events) == 0 && strings.TrimSpace(page.Summary) == ""
			}
		}
	}
	if len(page.Events) == 0 && strings.TrimSpace(page.Summary) != "" {
		page.Events = []LogEvent{{Kind: "output", Title: "Summary", Body: page.Summary}}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "job-log-content", page); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) loadJobLog(id string) JobLog {
	page := JobLog{ID: id}
	if !agentjob.ValidID(id) {
		page.Missing = true
		return page
	}

	st, err := agentjob.Open(filepath.Dir(s.historyPath))
	if err != nil {
		page.ReadError = "Could not open the job store."
		return page
	}

	found := false
	if j, err := st.Get(id); err == nil {
		found = true
		page.Title = j.Title
		page.Status = j.Status
		if j.RanWith != "" {
			page.Driver = j.RanWith
		} else {
			page.Driver = j.Driver
		}
		page.Model = j.Model
		page.PRURL = j.PRURL
		page.StartedAt = j.StartedAt
		page.FinishedAt = j.FinishedAt
		page.Duration = j.Duration
		page.Error = j.Error
		page.Summary = j.Summary
		page.Live = j.Status == agentjob.Running || j.Status == agentjob.Claimed
	} else if rec, ok := s.historyRecord(id); ok {
		found = true
		page.Title = rec.Title
		if page.Title == "" {
			page.Title = rec.Type
		}
		page.Status = rec.Status
		page.Driver = rec.Driver
		page.Model = rec.Model
		page.PRURL = rec.PRURL
		page.StartedAt = rec.StartedAt
		page.FinishedAt = rec.FinishedAt
		page.Duration = rec.Duration
		page.Error = rec.Error
		page.Summary = rec.Summary
		page.Live = rec.Status == "running"
	}

	if !found {
		page.Missing = true
		return page
	}

	// Don't load the transcript here — the page streams it from /api/jobs/{id}/log/stream.
	// Just check if a log file exists so we can warn if not.
	page.Empty = jobLogPath(st, id) == "" && strings.TrimSpace(page.Summary) == ""
	return page
}

// readJobTail reads at most the last max bytes of the job's transcript for
// the formatted view, dropping a partial leading line. max <= 0 reads the
// whole log. truncated reports whether older output was left out.
func readJobTail(st *agentjob.Store, id string, max int64) (raw, label string, truncated bool, err error) {
	path := jobLogPath(st, id)
	if path == "" {
		return "", "", false, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", "", false, err
	}
	if max > 0 && fi.Size() > max {
		truncated = true
		if _, err := f.Seek(fi.Size()-max, io.SeekStart); err != nil {
			return "", "", false, err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", "", false, err
	}
	if truncated {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return string(b), filepath.Base(path), truncated, nil
}

func (s *Server) historyRecord(id string) (JobRecord, bool) {
	for _, rec := range s.readRecentJobs(500) {
		if rec.ID == id {
			return rec, true
		}
	}
	return JobRecord{}, false
}

// ParseTranscript turns agent stdout into thoughts, commands, and outcomes.
func ParseTranscript(raw string) []LogEvent {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if ev, ok := parseJSONTranscript(raw); ok {
		return ev
	}
	if ev := parseTextTranscript(raw); len(ev) > 0 {
		return ev
	}
	return []LogEvent{{Kind: "output", Title: "Output", Body: raw}}
}

func parseJSONTranscript(raw string) ([]LogEvent, bool) {
	lines := strings.Split(raw, "\n")
	jsonLines, nonEmpty, hasDelta := 0, 0, false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(line, "{") {
			jsonLines++
			if !hasDelta && strings.Contains(line, `"content_block_delta"`) {
				hasDelta = true
			}
		}
	}
	if jsonLines == 0 || jsonLines*2 < nonEmpty {
		return nil, false
	}

	var events []LogEvent
	var textBuf strings.Builder
	// In partial-message transcripts the content_block_delta lines carry
	// each block token by token and the full assistant messages repeat it;
	// deltas are merged per block and those messages are skipped.
	var pendingKind string // "thought", "tool" or "text" (goes to textBuf)
	var pendingName string
	var pendingJSON strings.Builder
	flushText := func() {
		if t := strings.TrimSpace(textBuf.String()); t != "" {
			events = append(events, LogEvent{Kind: "output", Title: "Reply", Body: t})
			textBuf.Reset()
		}
	}
	flushPending := func() {
		switch pendingKind {
		case "thought":
			if t := strings.TrimSpace(pendingJSON.String()); t != "" {
				events = append(events, LogEvent{Kind: "thought", Title: "Thought", Body: t})
			}
			pendingJSON.Reset()
		case "tool":
			var input any
			if j := strings.TrimSpace(pendingJSON.String()); j != "" {
				json.Unmarshal([]byte(j), &input)
			}
			title := pendingName
			if title == "" {
				title = "Tool"
			}
			events = append(events, LogEvent{Kind: "command", Title: title, Body: formatToolInput(title, input)})
			pendingJSON.Reset()
		case "text":
			flushText()
		}
		pendingKind = ""
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			flushPending()
			flushText()
			events = append(events, LogEvent{Kind: "output", Title: "Output", Body: line})
			continue
		}
		typ, _ := ev["type"].(string)
		// --include-partial-messages wraps every partial event in a
		// stream_event envelope; unwrap it so the cases below see the
		// inner content_block_* event.
		if typ == "stream_event" {
			inner, ok := ev["event"].(map[string]any)
			if !ok {
				continue
			}
			ev = inner
			typ, _ = inner["type"].(string)
		}
		if hasDelta && typ == "assistant" {
			continue // already streamed via deltas
		}
		if hasDelta {
			switch typ {
			case "content_block_start":
				flushPending()
				b, _ := ev["content_block"].(map[string]any)
				switch b["type"] {
				case "thinking":
					pendingKind = "thought"
				case "tool_use":
					pendingKind = "tool"
					pendingName, _ = b["name"].(string)
				case "text":
					pendingKind = "text"
				}
			case "content_block_delta":
				d, _ := ev["delta"].(map[string]any)
				switch pendingKind {
				case "thought":
					if t, ok := d["thinking"].(string); ok {
						pendingJSON.WriteString(t)
					}
				case "text":
					if t, ok := d["text"].(string); ok {
						textBuf.WriteString(t)
					}
				case "tool":
					if t, ok := d["partial_json"].(string); ok {
						pendingJSON.WriteString(t)
					}
				}
			case "content_block_stop":
				flushPending()
			case "user", "result":
				flushPending()
				events = append(events, eventsFromJSON(ev, &textBuf)...)
				flushText()
				continue
			default:
				continue
			}
			continue
		}
		flushText()
		events = append(events, eventsFromJSON(ev, &textBuf)...)
	}
	flushPending()
	flushText()
	if len(events) == 0 {
		return nil, false
	}
	return events, true
}

func eventsFromJSON(ev map[string]any, textBuf *strings.Builder) []LogEvent {
	typ, _ := ev["type"].(string)
	switch typ {
	case "assistant":
		msg, _ := ev["message"].(map[string]any)
		return contentEvents(msg["content"], textBuf)
	case "user":
		msg, _ := ev["message"].(map[string]any)
		return toolResultEvents(msg["content"])
	case "content_block_delta":
		if d, ok := ev["delta"].(map[string]any); ok {
			if t, ok := d["text"].(string); ok {
				textBuf.WriteString(t)
			}
			if t, ok := d["thinking"].(string); ok && t != "" {
				return []LogEvent{{Kind: "thought", Title: "Thought", Body: t}}
			}
		}
	case "content_block_start":
		if b, ok := ev["content_block"].(map[string]any); ok {
			return contentEvents([]any{b}, textBuf)
		}
	case "result":
		if r, ok := ev["result"].(string); ok && strings.TrimSpace(r) != "" {
			return []LogEvent{{Kind: "output", Title: "Result", Body: r}}
		}
	}
	return nil
}

func contentEvents(content any, textBuf *strings.Builder) []LogEvent {
	var events []LogEvent
	blocks, _ := content.([]any)
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch b["type"] {
		case "thinking":
			if t, _ := b["thinking"].(string); strings.TrimSpace(t) != "" {
				events = append(events, LogEvent{Kind: "thought", Title: "Thought", Body: t})
			}
		case "text":
			if t, _ := b["text"].(string); strings.TrimSpace(t) != "" {
				textBuf.WriteString(t)
			}
		case "tool_use":
			name, _ := b["name"].(string)
			title := name
			if title == "" {
				title = "Tool"
			}
			events = append(events, LogEvent{Kind: "command", Title: title, Body: formatToolInput(name, b["input"])})
		}
	}
	return events
}

func toolResultEvents(content any) []LogEvent {
	blocks, _ := content.([]any)
	var events []LogEvent
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] != "tool_result" {
			continue
		}
		body := stringifyContent(b["content"])
		if strings.TrimSpace(body) == "" {
			continue
		}
		events = append(events, LogEvent{Kind: "result", Title: "Outcome", Body: body})
	}
	return events
}

func formatToolInput(name string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return stringifyContent(input)
	}
	for _, key := range []string{"command", "cmd", "bash"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	if name == "Read" || name == "read_file" {
		if v, ok := m["file_path"].(string); ok {
			return v
		}
		if v, ok := m["path"].(string); ok {
			return v
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return stringifyContent(input)
	}
	return string(b)
}

func stringifyContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if tx, ok := m["text"].(string); ok {
					parts = append(parts, tx)
					continue
				}
			}
			parts = append(parts, stringifyContent(item))
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		b, _ := json.MarshalIndent(t, "", "  ")
		return string(b)
	}
}

func parseTextTranscript(raw string) []LogEvent {
	lines := strings.Split(raw, "\n")
	var events []LogEvent
	var cur *LogEvent
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(cur.Body)
		if cur.Title != "" || cur.Body != "" {
			events = append(events, *cur)
		}
		cur = nil
	}
	for _, line := range lines {
		if m := textCommandRe.FindStringSubmatch(line); m != nil {
			flush()
			ev := LogEvent{Kind: "command", Title: m[1], Body: strings.TrimSpace(m[2])}
			cur = &ev
			continue
		}
		if m := textThoughtRe.FindStringSubmatch(line); m != nil {
			flush()
			ev := LogEvent{Kind: "thought", Title: "Thought", Body: strings.TrimSpace(m[1])}
			cur = &ev
			continue
		}
		if cur != nil && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") || strings.Contains(line, "⎿")) {
			body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "⎿"))
			if cur.Kind == "command" && body != "" {
				events = append(events, *cur)
				ev := LogEvent{Kind: "result", Title: "Outcome", Body: body}
				cur = &ev
			} else if body != "" {
				if cur.Body != "" {
					cur.Body += "\n"
				}
				cur.Body += body
			}
			continue
		}
		if cur != nil && strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if cur != nil && (cur.Kind == "thought" || cur.Kind == "output") {
			if cur.Body != "" {
				cur.Body += "\n"
			}
			cur.Body += line
			continue
		}
		flush()
		if strings.TrimSpace(line) == "" {
			continue
		}
		ev := LogEvent{Kind: "output", Title: "Output", Body: line}
		cur = &ev
	}
	flush()
	return events
}

var (
	textCommandRe = regexp.MustCompile(`^[●•]\s+([A-Za-z][\w./-]*)\((.*)\)\s*$`)
	textThoughtRe = regexp.MustCompile(`^(?:Thinking:|💭)\s*(.*)$`)
)
