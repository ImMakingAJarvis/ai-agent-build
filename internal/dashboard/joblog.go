package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// LogEvent is one chronological step on a job log page.
type LogEvent struct {
	Kind  string // thought, command, result, output
	Title string
	Body  string
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
	SourceLabel string
}

func jobLogFuncs() template.FuncMap {
	return template.FuncMap{
		"statusClass":    StatusClass,
		"formatDuration": FormatDuration,
		"formatCost":     FormatCost,
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
	tmpl, err := template.New("job-log.html").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "job-log.html", page); err != nil {
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
	tmpl, err := template.New("job-log-content").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "job-log-content", page); err != nil {
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

	raw, src, err := readJobTrace(st, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		page.ReadError = "Could not read this job's log from disk."
		return page
	}
	if !found && errors.Is(err, os.ErrNotExist) {
		page.Missing = true
		return page
	}

	page.SourceLabel = src
	page.Events = ParseTranscript(raw)
	if len(page.Events) == 0 && strings.TrimSpace(page.Summary) != "" {
		page.Events = []LogEvent{{Kind: "output", Title: "Summary", Body: page.Summary}}
		if page.SourceLabel == "" {
			page.SourceLabel = "jobs.jsonl summary"
		}
	}
	page.Empty = len(page.Events) == 0
	return page
}

func readJobTrace(st *agentjob.Store, id string) (raw, label string, err error) {
	b, err := st.ReadFile(id, "transcript.txt")
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return string(b), "transcript.txt", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	b, err = st.ReadFile(id, "output.txt")
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return string(b), "output.txt", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
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
	jsonLines, nonEmpty := 0, 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(line, "{") {
			jsonLines++
		}
	}
	if jsonLines == 0 || jsonLines*2 < nonEmpty {
		return nil, false
	}

	var events []LogEvent
	var textBuf strings.Builder
	flushText := func() {
		if t := strings.TrimSpace(textBuf.String()); t != "" {
			events = append(events, LogEvent{Kind: "output", Title: "Reply", Body: t})
			textBuf.Reset()
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			flushText()
			events = append(events, LogEvent{Kind: "output", Title: "Output", Body: line})
			continue
		}
		events = append(events, eventsFromJSON(ev, &textBuf)...)
	}
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
